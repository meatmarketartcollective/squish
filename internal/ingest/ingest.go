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
	// Chunk opts in to experimental content-defined chunking for files
	// at or above the chunk threshold. Off by default: on the real
	// corpus the chunk-blob path stores more than whole-file blobs
	// (per-chunk compression windows), so E1 stays behind this flag.
	Chunk bool
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
			var chunks []store.ChunkRef
			fh, chunks, err := putFile(st, data, opts.Chunk)
			if err != nil {
				return err
			}
			e.Hash = fh
			if len(chunks) > 0 {
				e.Kind = store.KindChunked
				e.Chunks = chunks
			} else {
				e.Kind = store.KindFile
			}
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
	// Capture the image config so restore can rebuild a docker-loadable
	// archive from a store-made manifest, not just reclaim-made ones.
	var configName string
	if len(entries) == 1 {
		configName = cleanName(entries[0].Config)
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
		if configName != "" && n == configName {
			cfg, err := io.ReadAll(tr2)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", n, err)
			}
			// Same validity bar reclaim applies before it will ever
			// delete an image: the config must parse and carry diff_ids.
			var cfgCheck struct {
				Rootfs struct {
					DiffIds []string `json:"diff_ids"`
				} `json:"rootfs"`
			}
			if json.Valid(cfg) && json.Unmarshal(cfg, &cfgCheck) == nil && cfgCheck.Rootfs.DiffIds != nil {
				if hash, perr := st.PutBlob(cfg); perr == nil {
					m.ImageConfig = hash
				}
			}
			io.Copy(io.Discard, tr2)
			continue
		}
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

// putFile stores file content: whole-file blob normally, or chunked
// into content-defined pieces when opted in.
func putFile(st *store.Store, data []byte, chunk bool) (string, []store.ChunkRef, error) {
	if chunk {
		return st.PutFileContent(data)
	}
	hash, err := st.PutBlob(data)
	return hash, nil, err
}

// tallyDedup counts occurrences beyond the first per content hash,
// whole files and chunks alike.
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
	seenChunk := map[string]bool{}
	for _, e := range m.Files {
		if e.Kind == store.KindChunked {
			for _, c := range e.Chunks {
				if seenChunk[c.Hash] {
					m.ChunkDedupBytes += c.Size
				} else {
					seenChunk[c.Hash] = true
				}
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
	n = strings.TrimPrefix(n, "/")
	return strings.TrimSuffix(n, "/")
}

// checkSafe rejects layer paths that would escape the layer root when
// materialized ("../" components); such a layer is malformed or hostile.
func checkSafe(n string) error {
	for _, part := range strings.Split(n, "/") {
		if part == ".." {
			return fmt.Errorf("layer contains unsafe path %q", n)
		}
	}
	return nil
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
		if err := checkSafe(n); err != nil {
			return err
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
			var chunks []store.ChunkRef
			fh, chunks, err := putFile(st, data, opts.Chunk)
			if err != nil {
				return err
			}
			e.Hash = fh
			if len(chunks) > 0 {
				e.Kind = store.KindChunked
				e.Chunks = chunks
			} else {
				e.Kind = store.KindFile
			}
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
