//go:build !linux

package ops

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aloysiusvonfck/squish/internal/store"
)

// materializeFallback is the pre-secure path-based materializer, kept for
// platforms (macOS) without the Linux openat/O_NOFOLLOW backend. It keeps
// the same guards the secure path keeps out-of-band: safeJoin refuses
// manifest paths that escape the extraction root, and ensureInsideOut
// re-checks that every created directory resolved back inside it. It does
// not defend against a symlink swapped into a parent between these checks,
// which is why the Linux build uses the secure backend instead.
func materializeFallback(st *store.Store, m *store.Manifest, out string, useHardlinks, verifyEach bool) (files int, err error) {
	first := map[string]string{}   // hash -> first materialized path
	written := map[string]string{} // manifest path -> path on disk
	byPath := map[string]store.FileEntry{}
	var links []store.FileEntry // hardlinks, resolved after their targets
	for _, e := range m.Files {
		dst, jerr := safeJoin(out, e.Path)
		if jerr != nil {
			return files, jerr
		}
		switch e.Kind {
		case store.KindDir:
			if err := os.MkdirAll(dst, os.FileMode(e.Mode)|0o700); err != nil {
				return files, err
			}
			if err := ensureInsideOut(out, dst); err != nil {
				return files, err
			}
		case store.KindFile, store.KindChunked, store.KindFrame, store.KindDelta, store.KindCold:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return files, err
			}
			if err := ensureInsideOut(out, filepath.Dir(dst)); err != nil {
				return files, err
			}
			data, err := readEntryData(st, e)
			if err != nil {
				return files, fmt.Errorf("%s: %w", e.Path, err)
			}
			if int64(len(data)) != e.Size {
				return files, fmt.Errorf("%s: size mismatch (%d != %d)", e.Path, len(data), e.Size)
			}
			if prev, ok := first[e.Hash]; ok && useHardlinks {
				if err := os.Link(prev, dst); err != nil {
					// cross-device or unsupported: fall back to a copy
					if werr := os.WriteFile(dst, data, os.FileMode(e.Mode)); werr != nil {
						return files, werr
					}
				}
			} else {
				if err := os.WriteFile(dst, data, os.FileMode(e.Mode)); err != nil {
					return files, err
				}
				first[e.Hash] = dst
			}
			written[e.Path] = dst
			byPath[e.Path] = e
			if verifyEach {
				if err := checkFile(dst, e); err != nil {
					return files, err
				}
			}
			files++
		case store.KindSymlink:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return files, err
			}
			os.Remove(dst)
			if err := os.Symlink(e.Link, dst); err != nil {
				return files, err
			}
			written[e.Path] = dst
		case store.KindLink:
			links = append(links, e)
		case store.KindElided, store.KindSpecial:
			// nothing on disk; parent dirs are created on demand
		}
	}
	// hardlinks: their targets are on disk by now (tar hardlinks always
	// point inside the same layer, so the target is in this manifest).
	// A hardlink may point at another hardlink; resolve each chain to its
	// ultimate non-link target and refuse cycles.
	linkTarget := make(map[string]string, len(links))
	for _, le := range links {
		linkTarget[le.Path] = le.Link
	}
	resolveBase := func(path string) (string, bool) {
		seen := map[string]bool{}
		for {
			if seen[path] {
				return "", false
			}
			seen[path] = true
			if t, isLink := linkTarget[path]; isLink {
				path = t
				continue
			}
			return path, true
		}
	}
	for _, le := range links {
		dst, jerr := safeJoin(out, le.Path)
		if jerr != nil {
			return files, jerr
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return files, err
		}
		if err := ensureInsideOut(out, filepath.Dir(dst)); err != nil {
			return files, err
		}
		base, ok := resolveBase(le.Link)
		if !ok {
			return files, fmt.Errorf("%s: hardlink chain through %q never reaches a file", le.Path, le.Link)
		}
		tgt, ok := written[base]
		if !ok {
			return files, fmt.Errorf("%s: hardlink target %q was not materialized", le.Path, base)
		}
		os.Remove(dst)
		if err := os.Link(tgt, dst); err != nil {
			// linkless filesystem (Android FUSE and friends): fall
			// back to writing the target's content
			te, ok := byPath[base]
			if !ok {
				return files, fmt.Errorf("%s: hardlink to %s: %w", le.Path, base, err)
			}
			data, rerr := readEntryData(st, te)
			if rerr != nil {
				return files, rerr
			}
			if werr := os.WriteFile(dst, data, os.FileMode(le.Mode)); werr != nil {
				return files, werr
			}
		}
		files++
	}
	return files, nil
}
