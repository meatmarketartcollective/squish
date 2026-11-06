// Package ingest reads Docker image sources into the store.
package ingest

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aloysiusvonfck/squish/internal/elide"
	"github.com/aloysiusvonfck/squish/internal/store"
)

// IngestOptions controls one ingest run.
type IngestOptions struct {
	Elide  bool
	NoGzip bool // caller already wrapped the reader
}

// IngestPath ingests a docker save tarball (.tar or .tar.gz) or a plain
// directory tree into the store and returns a manifest (not yet saved).
func IngestPath(st *store.Store, src, name string, opts IngestOptions) (*store.Manifest, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		m, err := ingestDir(st, src, name, opts)
		if err != nil {
			return nil, err
		}
		return tallyDedup(m), nil
	}
	m, err := ingestDockerSave(st, src, name, opts)
	if err != nil {
		return nil, err
	}
	return tallyDedup(m), nil
}

func ingestDir(st *store.Store, root, name string, opts IngestOptions) (*store.Manifest, error) {
	m := &store.Manifest{Name: name, Source: "dir:" + root}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			m.Files = append(m.Files, store.FileEntry{Path: rel, Mode: uint32(info.Mode().Perm()), Kind: store.KindDir})
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			m.Files = append(m.Files, store.FileEntry{Path: rel, Mode: 0o777, Kind: store.KindSymlink, Link: link})
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			m.RawBytes += int64(len(data))
			e := store.FileEntry{Path: rel, Size: int64(len(data)), Mode: uint32(info.Mode().Perm())}
			if opts.Elide {
				if reason := elide.Match(rel, int64(len(data))); reason != "" {
					e.Kind = store.KindElided
					e.Reason = reason
					m.ElidedBytes += int64(len(data))
					m.Files = append(m.Files, e)
					return nil
				}
			}
			hash, err := st.PutBlob(data)
			if err != nil {
				return err
			}
			e.Kind = store.KindFile
			e.Hash = hash
			m.Files = append(m.Files, e)
		default:
			m.Files = append(m.Files, store.FileEntry{Path: rel, Kind: store.KindSpecial})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	store.SortEntries(m.Files)
	return m, nil
}

type dockerManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// ingestDockerSave reads a docker save archive: manifest.json plus ordered
// layer tars, applying overlay whiteouts to compute the final tree.
func ingestDockerSave(st *store.Store, path, name string, opts IngestOptions) (*store.Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var raw io.Reader = f
	closeGz := func() {}
	if strings.HasSuffix(strings.ToLower(path), ".gz") && !opts.NoGzip {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		raw = gz
		closeGz = func() { gz.Close() }
	}
	defer closeGz()

	// Pass 1: read manifest.json (streamed; other entries skipped).
	var entries []dockerManifestEntry
	tr := tar.NewReader(raw)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if cleanName(hdr.Name) == "manifest.json" {
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(b, &entries); err != nil {
				return nil, fmt.Errorf("manifest.json: %w", err)
			}
			break // manifest precedes the layers in docker save archives
		}
		io.Copy(io.Discard, tr)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no manifest.json found in %s", path)
	}

	m := &store.Manifest{Name: name, Source: "docker-save:" + path}
	if len(entries) == 1 && len(entries[0].RepoTags) > 0 {
		m.Source = "docker-save:" + entries[0].RepoTags[0]
	}
	for _, e := range entries {
		m.Layers = append(m.Layers, e.Layers...)
	}

	// Pass 2: re-open and process each layer tar in order.
	closeGz()
	if err := f.Close(); err != nil {
		return nil, err
	}
	f2, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f2.Close()
	raw2 := io.Reader(f2)
	if strings.HasSuffix(strings.ToLower(path), ".gz") && !opts.NoGzip {
		gz2, err := gzip.NewReader(f2)
		if err != nil {
			return nil, err
		}
		defer gz2.Close()
		raw2 = gz2
	}
	tr2 := tar.NewReader(raw2)
	layerSet := map[string]bool{}
	for _, l := range m.Layers {
		layerSet[l] = true
		layerSet["./"+l] = true
	}
	state := map[string]store.FileEntry{}
	for {
		hdr, err := tr2.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		n := cleanName(hdr.Name)
		if !layerSet[n] && !hasLayerSuffix(layerSet, n) {
			io.Copy(io.Discard, tr2)
			continue
		}
		lr := tar.NewReader(tr2)
		if err := applyLayer(lr, state, m, st, opts); err != nil {
			return nil, err
		}
	}

	for _, e := range state {
		m.Files = append(m.Files, e)
	}
	store.SortEntries(m.Files)

	return tallyDedup(m), nil
}

// tallyDedup counts occurrences beyond the first per content hash.
func tallyDedup(m *store.Manifest) *store.Manifest {
	seen := map[string]int{}
	for _, e := range m.Files {
		if e.Kind == store.KindFile {
			seen[e.Hash]++
			if seen[e.Hash] > 1 {
				m.DedupBytes += e.Size
			}
		}
	}
	return m
}

func hasLayerSuffix(layerSet map[string]bool, n string) bool {
	for l := range layerSet {
		if strings.HasSuffix(n, l) || strings.HasSuffix(n, strings.TrimPrefix(l, "./")) {
			return true
		}
	}
	return false
}

func cleanName(n string) string {
	n = strings.TrimPrefix(n, "./")
	return strings.TrimSuffix(n, "/")
}

func applyLayer(lr *tar.Reader, state map[string]store.FileEntry, m *store.Manifest, st *store.Store, opts IngestOptions) error {
	for {
		hdr, err := lr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		n := cleanName(hdr.Name)
		if n == "" {
			continue
		}
		base := filepath.Base(n)
		dir := filepath.Dir(n)

		// Overlay whiteouts.
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeChar {
			if base == ".wh..wh..opq" {
				for p := range state {
					if dir == "." || strings.HasPrefix(p, dir+"/") {
						delete(state, p)
					}
				}
				continue
			}
			if strings.HasPrefix(base, ".wh.") {
				delete(state, filepath.Join(dir, base[4:]))
				continue
			}
		}

		switch hdr.Typeflag {
		case tar.TypeReg:
			data, err := io.ReadAll(io.LimitReader(lr, hdr.Size))
			if err != nil {
				return err
			}
			if int64(len(data)) != hdr.Size {
				return fmt.Errorf("short read on %s", n)
			}
			m.RawBytes += int64(len(data))
			e := store.FileEntry{Path: n, Size: int64(len(data)), Mode: uint32(hdr.Mode & 0o7777)}
			if opts.Elide {
				if reason := elide.Match(n, int64(len(data))); reason != "" {
					e.Kind = store.KindElided
					e.Reason = reason
					m.ElidedBytes += int64(len(data))
					state[n] = e
					continue
				}
			}
			hash, err := st.PutBlob(data)
			if err != nil {
				return err
			}
			e.Kind = store.KindFile
			e.Hash = hash
			state[n] = e
		case tar.TypeSymlink:
			state[n] = store.FileEntry{Path: n, Mode: 0o777, Kind: store.KindSymlink, Link: hdr.Linkname}
		case tar.TypeLink:
			state[n] = store.FileEntry{Path: n, Mode: uint32(hdr.Mode & 0o7777), Kind: store.KindLink, Link: cleanName(hdr.Linkname)}
		case tar.TypeDir:
			state[n] = store.FileEntry{Path: n, Mode: uint32(hdr.Mode & 0o7777), Kind: store.KindDir}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			state[n] = store.FileEntry{Path: n, Kind: store.KindSpecial}
		default:
			// ignore unknown types
		}
	}
}

// SortPaths is a helper for tests.
func SortPaths(ps []string) { sort.Strings(ps) }
