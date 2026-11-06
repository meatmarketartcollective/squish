// Package ops implements materialize, verify, repack and stats.
package ops

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/aloysiusvonfck/stackfold/internal/store"
)

// Materialize reconstructs the image tree at out.
func Materialize(st *store.Store, m *store.Manifest, out string, useHardlinks, verifyEach bool) (files int, err error) {
	first := map[string]string{} // hash -> first materialized path
	for _, e := range m.Files {
		dst := filepath.Join(out, filepath.FromSlash(e.Path))
		switch e.Kind {
		case store.KindDir:
			if err := os.MkdirAll(dst, os.FileMode(e.Mode)|0o700); err != nil {
				return files, err
			}
		case store.KindFile, store.KindFrame:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return files, err
			}
			var data []byte
			var err error
			if e.Kind == store.KindFile {
				data, err = st.ReadBlob(e.Hash)
			} else {
				data, err = st.ReadFrameFile(e.Frame, e.Offset, e.Clen)
			}
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
		case store.KindElided, store.KindSpecial:
			// nothing on disk; parent dirs are created on demand
		}
	}
	return files, nil
}

func checkFile(dst string, e store.FileEntry) error {
	data, err := os.ReadFile(dst)
	if err != nil {
		return err
	}
	hash := store.HashBytes(data)
	if hash != e.Hash {
		return fmt.Errorf("%s: hash mismatch after materialize", e.Path)
	}
	return nil
}

// Verify re-derives every file's hash from the store.
func Verify(st *store.Store, m *store.Manifest) (checked, bad int, err error) {
	for _, e := range m.Files {
		switch e.Kind {
		case store.KindFile:
			data, err := st.ReadBlob(e.Hash)
			if err != nil {
				return checked, bad, err
			}
			checked++
			if store.HashBytes(data) != e.Hash {
				bad++
			}
		case store.KindFrame:
			data, err := st.ReadFrameFile(e.Frame, e.Offset, e.Clen)
			if err != nil {
				return checked, bad, fmt.Errorf("%s: %w", e.Path, err)
			}
			checked++
			if int64(len(data)) != e.Size || store.HashBytes(data) != e.Hash {
				bad++
			}
		}
	}
	return checked, bad, nil
}

// Stats summarizes a manifest against the store.
func Stats(st *store.Store, m *store.Manifest) error {
	blobBytes := map[string]int64{}
	frameBytes := map[string]int64{}
	var stored int64
	for _, e := range m.Files {
		switch e.Kind {
		case store.KindFile:
			if _, ok := blobBytes[e.Hash]; !ok {
				sz, err := st.BlobSize(e.Hash)
				if err != nil {
					return err
				}
				blobBytes[e.Hash] = sz
				stored += sz
			}
		case store.KindFrame:
			if _, ok := frameBytes[e.Frame]; !ok {
				for _, p := range []string{st.FramePath(e.Frame), st.FrameDictPath(e.Frame), st.FrameIndexPath(e.Frame)} {
					fi, err := os.Stat(p)
					if err != nil {
						return err
					}
					stored += fi.Size()
				}
				frameBytes[e.Frame] = 1
			}
		}
	}
	pct := 0.0
	if m.RawBytes > 0 {
		pct = 100 * float64(stored) / float64(m.RawBytes)
	}
	fmt.Printf("image           %s\n", m.Name)
	fmt.Printf("files           %d\n", len(m.Files))
	fmt.Printf("raw content     %d B\n", m.RawBytes)
	fmt.Printf("elided (regen)  %d B\n", m.ElidedBytes)
	fmt.Printf("dedup savings   %d B\n", m.DedupBytes)
	fmt.Printf("stored          %d B (%.1f%% of raw)\n", stored, pct)
	return nil
}

// Repack applies the measured offline win: small same-directory files are
// compressed at deep settings with a shared dictionary built from the group,
// keeping per-file random access through the frame index.
func Repack(st *store.Store, m *store.Manifest, maxFile, minGroup int, force bool) (groups, moved int, err error) {
	byDir := map[string][]*store.FileEntry{}
	for i := range m.Files {
		e := &m.Files[i]
		if e.Kind == store.KindFile && e.Size <= int64(maxFile) {
			byDir[filepath.Dir(e.Path)] = append(byDir[filepath.Dir(e.Path)], e)
		}
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		es := byDir[d]
		if len(es) < minGroup {
			continue
		}
		sort.Slice(es, func(i, j int) bool { return es[i].Path < es[j].Path })
		gid := "f" + store.RandomID(8)
		dict, err := buildDict(st, es)
		if err != nil {
			return groups, moved, err
		}
		var payload []byte
		var idx []store.FrameFileEntry
		for _, e := range es {
			raw, err := st.ReadBlob(e.Hash)
			if err != nil {
				return groups, moved, err
			}
			comp, err := encodeWithDict(raw, dict)
			if err != nil {
				return groups, moved, err
			}
			idx = append(idx, store.FrameFileEntry{Path: e.Path, Offset: int64(len(payload)), Clen: int64(len(comp)), Size: e.Size})
			payload = append(payload, comp...)
		}
		if err := st.WriteFrame(gid, dict, idx, payload); err != nil {
			return groups, moved, err
		}
		for _, e := range es {
			e.Kind = store.KindFrame
			e.Frame = gid
			e.Offset = offsetOf(idx, e.Path)
			e.Clen = clenOf(idx, e.Path)
		}
		groups++
		moved += len(es)
	}
	if err := st.SaveManifest(m); err != nil {
		return groups, moved, err
	}
	n, err := st.DeleteOrphanBlobs(m, force)
	if err != nil {
		return groups, moved, err
	}
	fmt.Printf("repacked %d groups (%d files) into dictionary frames, %d blobs reclaimed\n", groups, moved, n)
	return groups, moved, nil
}

func offsetOf(idx []store.FrameFileEntry, p string) int64 {
	for _, f := range idx {
		if f.Path == p {
			return f.Offset
		}
	}
	return 0
}
func clenOf(idx []store.FrameFileEntry, p string) int64 {
	for _, f := range idx {
		if f.Path == p {
			return f.Clen
		}
	}
	return 0
}

// buildDict concatenates up to 4 of the group's largest files (capped) as a
// raw-content dictionary: context shared across the batch without a trainer.
func buildDict(st *store.Store, es []*store.FileEntry) ([]byte, error) {
	sorted := make([]*store.FileEntry, len(es))
	copy(sorted, es)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Size > sorted[j].Size })
	var dict []byte
	for i := 0; i < len(sorted) && i < 4 && len(dict) < 96*1024; i++ {
		raw, err := st.ReadBlob(sorted[i].Hash)
		if err != nil {
			return nil, err
		}
		if len(dict)+len(raw) > 96*1024 {
			raw = raw[:96*1024-len(dict)]
		}
		dict = append(dict, raw...)
	}
	if len(dict) == 0 {
		return nil, fmt.Errorf("empty dictionary")
	}
	return dict, nil
}

func encodeWithDict(data, dict []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderDictRaw(1, dict),
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithWindowSize(1<<27),
	)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil), nil
}

var _ = json.Marshal
var _ = strings.TrimSpace
var _ = io.Discard
