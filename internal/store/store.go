// Package store implements Squish's per-file content-addressed store.
package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/blake2b"
)

const StoreVersion = 2

// Kinds for manifest entries.
const (
	KindDir     = "dir"
	KindFile    = "file"    // unique blob in the store
	KindChunked = "chunked" // file stored as content-defined chunk blobs
	KindFrame   = "frame"
	KindSymlink = "symlink"
	KindLink    = "hardlink"
	KindElided  = "elided"
	KindSpecial = "special"
)

type FileEntry struct {
	Path   string     `json:"path"`
	Size   int64      `json:"size"`
	Mode   uint32     `json:"mode"`
	Kind   string     `json:"kind"`
	Hash   string     `json:"hash,omitempty"`
	Link   string     `json:"link,omitempty"`
	Frame  string     `json:"frame,omitempty"`
	Base   string     `json:"base,omitempty"`
	Filter string     `json:"filter,omitempty"`
	Offset int64      `json:"offset,omitempty"`
	Clen   int64      `json:"clen,omitempty"`
	Reason string     `json:"reason,omitempty"`
	Chunks []ChunkRef `json:"chunks,omitempty"`
}

// ChunkRef points at one content-defined chunk of a chunked file. At
// ingest the chunk is a plain blob; after repack it lives inside the
// manifest's chunk frame at Offset/Clen.
type ChunkRef struct {
	Hash   string `json:"hash"`
	Size   int64  `json:"size"`
	Frame  string `json:"frame,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	Clen   int64  `json:"clen,omitempty"`
}

type Manifest struct {
	Version         int         `json:"version"`
	Name            string      `json:"name"`
	Created         time.Time   `json:"created"`
	Source          string      `json:"source"`
	Layers          []string    `json:"layers,omitempty"`
	Files           []FileEntry `json:"files"`
	RawBytes        int64       `json:"raw_bytes"`
	DedupBytes      int64       `json:"dedup_bytes"`
	ElidedBytes     int64       `json:"elided_bytes"`
	StoredBytes     int64       `json:"stored_bytes"`
	ChunkDedupBytes int64       `json:"chunk_dedup_bytes,omitempty"`

	// reclaim/restore: original image config blob hash and repo tags
	ImageConfig string   `json:"image_config,omitempty"`
	RepoTags    []string `json:"repo_tags,omitempty"`
}

type FrameIndex struct {
	GID   string           `json:"gid"`
	Files []FrameFileEntry `json:"files"`
}

type FrameFileEntry struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Clen   int64  `json:"clen"`
	Size   int64  `json:"size"`
}

type Store struct {
	Root string
	// Fast opts out of the default best-compression write path: when
	// true, new blobs, frame indexes and manifests are written at the
	// fast level instead of max. Reads are unaffected; zstd frames
	// decode identically at either level.
	Fast bool
}

func Open(root string) (*Store, error) {
	for _, d := range []string{"", "blobs", "frames", "manifests"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{Root: root}, nil
}

func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.Root, "blobs", hash[:2], hash+".zst")
}

func (s *Store) FramePath(gid string) string     { return filepath.Join(s.Root, "frames", gid+".zst") }
func (s *Store) FrameDictPath(gid string) string { return filepath.Join(s.Root, "frames", gid+".dict") }
func (s *Store) FrameIndexPath(gid string) string {
	return filepath.Join(s.Root, "frames", gid+".idx.zst")
}

// legacyFrameIndexPath is where frame indexes lived before store v2.
func (s *Store) legacyFrameIndexPath(gid string) string {
	return filepath.Join(s.Root, "frames", gid+".idx.json")
}

func (s *Store) ManifestPath(name string) string {
	return filepath.Join(s.Root, "manifests", name+".json.zst")
}

// legacyManifestPath is where manifests lived before store v2.
func (s *Store) legacyManifestPath(name string) string {
	return filepath.Join(s.Root, "manifests", name+".json")
}

func HashBytes(data []byte) string {
	h, _ := blake2b.New256(nil)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func RandomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// encOpts returns the fast-store encoder settings: zstd level 3 equivalent,
// 128 MB window so the long-range matcher sees across a file.
func encOpts(level zstd.EncoderLevel) []zstd.EOption {
	return []zstd.EOption{
		zstd.WithEncoderLevel(level),
		zstd.WithWindowSize(1 << 27),
		zstd.WithZeroFrames(true),
	}
}

func decOpts() []zstd.DOption {
	return []zstd.DOption{zstd.WithDecoderMaxWindow(1 << 27)}
}

// EncodeAllFast compresses data at the fast level (the old install-path
// setting, now the -fast escape hatch).
func EncodeAllFast(data []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, encOpts(zstd.SpeedDefault)...)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil), nil
}

// EncodeAllMax compresses data at the best-compression level with the
// same 128 MB window: the default for new objects since v0.3.11.
// Decompression cost is level-independent, so reads stay as fast.
func EncodeAllMax(data []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, encOpts(zstd.SpeedBestCompression)...)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil), nil
}

// encodeAll writes new objects at the store's configured level: max
// compression by default, fast when Store.Fast is set.
func (s *Store) encodeAll(data []byte) ([]byte, error) {
	if s.Fast {
		return EncodeAllFast(data)
	}
	return EncodeAllMax(data)
}

// DecodeAll decompresses a plain (undictionaried) zstd frame.
func DecodeAll(src []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil, decOpts()...)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(src, nil)
}

// putBlob stores data under its content hash unless the blob already exists
// (dedup, including across manifests sharing the store). Returns the hash.
func (s *Store) PutBlob(data []byte) (string, error) {
	hash := HashBytes(data)
	p := s.blobPath(hash)
	if _, err := os.Stat(p); err == nil {
		return hash, nil
	}
	comp, err := s.encodeAll(data)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	tmp := p + ".tmp" + RandomID(4)
	if err := os.WriteFile(tmp, comp, 0o644); err != nil {
		return "", err
	}
	return hash, os.Rename(tmp, p)
}

func (s *Store) ReadBlob(hash string) ([]byte, error) {
	comp, err := os.ReadFile(s.blobPath(hash))
	if err != nil {
		return nil, err
	}
	return DecodeAll(comp)
}

func (s *Store) BlobSize(hash string) (int64, error) {
	fi, err := os.Stat(s.blobPath(hash))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (s *Store) WriteFrame(gid string, dict []byte, files []FrameFileEntry, payload []byte) error {
	if err := os.WriteFile(s.FrameDictPath(gid), dict, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(s.FramePath(gid), payload, 0o644); err != nil {
		return err
	}
	idx := FrameIndex{GID: gid, Files: files}
	b, _ := json.Marshal(idx)
	comp, err := s.encodeAll(b)
	if err != nil {
		return err
	}
	tmp := s.FrameIndexPath(gid) + ".tmp" + RandomID(4)
	if err := os.WriteFile(tmp, comp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.FrameIndexPath(gid)); err != nil {
		return err
	}
	_ = os.Remove(s.legacyFrameIndexPath(gid))
	return nil
}

// ReadFrameIndex loads a frame index, reading the compressed v2 form or
// the legacy plain-JSON form.
func (s *Store) ReadFrameIndex(gid string) (*FrameIndex, error) {
	b, err := os.ReadFile(s.FrameIndexPath(gid))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		b, err = os.ReadFile(s.legacyFrameIndexPath(gid))
		if err != nil {
			return nil, err
		}
	} else {
		if b, err = DecodeAll(b); err != nil {
			return nil, fmt.Errorf("frame index %s: %w", gid, err)
		}
	}
	var idx FrameIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func (s *Store) ReadFrameFile(gid string, off, clen int64) ([]byte, error) {
	f, err := os.Open(s.FramePath(gid))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	chunk := make([]byte, clen)
	if _, err := f.ReadAt(chunk, off); err != nil {
		return nil, err
	}
	dict, err := os.ReadFile(s.FrameDictPath(gid))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, append(decOpts(), zstd.WithDecoderDictRaw(1, dict))...)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(chunk, nil)
}

// ReadChunked reassembles a chunked file from its chunk list, reading
// each chunk from its blob or, after repack, from its frame. Chunks are
// visited in file order so the read stays sequential over the file's
// own chunks.
func (s *Store) ReadChunked(e *FileEntry) ([]byte, error) {
	out := make([]byte, 0, e.Size)
	for i := range e.Chunks {
		c := &e.Chunks[i]
		var data []byte
		var err error
		if c.Frame != "" {
			data, err = s.ReadFrameFile(c.Frame, c.Offset, c.Clen)
		} else {
			data, err = s.ReadBlob(c.Hash)
		}
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != c.Size {
			return nil, fmt.Errorf("chunk %s: size mismatch (%d != %d)", c.Hash[:12], len(data), c.Size)
		}
		out = append(out, data...)
	}
	return out, nil
}

// PutFileContent stores data as one whole-file blob (small files) or as
// content-defined chunks (files at least ChunkMinFile bytes), returning
// the file's whole-content hash and its chunk list. Chunks and files
// share the blob store, so dedup spans both.
func (s *Store) PutFileContent(data []byte) (string, []ChunkRef, error) {
	fh := HashBytes(data)
	if len(data) < ChunkMinFile {
		if _, err := s.PutBlob(data); err != nil {
			return "", nil, err
		}
		return fh, nil, nil
	}
	parts := ChunkData(data, MinChunkSize, AvgChunkSize, MaxChunkSize)
	refs := make([]ChunkRef, 0, len(parts))
	for _, p := range parts {
		h, err := s.PutBlob(p)
		if err != nil {
			return "", nil, err
		}
		refs = append(refs, ChunkRef{Hash: h, Size: int64(len(p))})
	}
	return fh, refs, nil
}

// SaveManifest writes the manifest JSON atomically and computes StoredBytes.
func (s *Store) SaveManifest(m *Manifest) error {
	seenBlob := map[string]bool{}
	seenFrame := map[string]bool{}
	seenDelta := map[string]bool{}
	seenCold := map[string]bool{}
	var stored int64
	for _, e := range m.Files {
		switch e.Kind {
		case KindFile:
			if !seenBlob[e.Hash] {
				seenBlob[e.Hash] = true
				sz, err := s.BlobSize(e.Hash)
				if err != nil {
					return err
				}
				stored += sz
			}
		case KindDelta:
			key := e.Hash + ">" + e.Base
			if !seenDelta[key] {
				seenDelta[key] = true
				sz, err := s.DeltaSize(e.Hash, e.Base)
				if err != nil {
					return err
				}
				stored += sz
			}
		case KindCold:
			if !seenCold[e.Hash] {
				seenCold[e.Hash] = true
				sz, err := s.ColdSize(e.Hash)
				if err != nil {
					return err
				}
				stored += sz
			}
		case KindChunked:
			for _, c := range e.Chunks {
				if c.Frame != "" {
					if !seenFrame[c.Frame] {
						seenFrame[c.Frame] = true
						sz, err := s.frameBytes(c.Frame)
						if err != nil {
							return err
						}
						stored += sz
					}
					continue
				}
				if !seenBlob[c.Hash] {
					seenBlob[c.Hash] = true
					sz, err := s.BlobSize(c.Hash)
					if err != nil {
						return err
					}
					stored += sz
				}
			}
		case KindFrame:
			if !seenFrame[e.Frame] {
				seenFrame[e.Frame] = true
				sz, err := s.frameBytes(e.Frame)
				if err != nil {
					return err
				}
				stored += sz
			}
		}
	}
	m.StoredBytes = stored
	m.Version = StoreVersion
	m.Created = time.Now().UTC()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	comp, err := s.encodeAll(b)
	if err != nil {
		return err
	}
	tmp := s.ManifestPath(m.Name) + ".tmp" + RandomID(4)
	if err := os.WriteFile(tmp, comp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.ManifestPath(m.Name)); err != nil {
		return err
	}
	// store v2 keeps manifests compressed; drop any legacy plain copy
	_ = os.Remove(s.legacyManifestPath(m.Name))
	return nil
}

// FrameBytes totals a frame's on-disk footprint: payload, dictionary
// and (compressed) index.
func (s *Store) FrameBytes(gid string) (int64, error) {
	var total int64
	for _, p := range []string{s.FramePath(gid), s.FrameDictPath(gid), s.FrameIndexPath(gid)} {
		fi, err := os.Stat(p)
		if err != nil {
			return 0, err
		}
		total += fi.Size()
	}
	return total, nil
}

// frameBytes totals a frame's on-disk footprint: payload, dictionary
// and (compressed) index.
func (s *Store) frameBytes(gid string) (int64, error) {
	var total int64
	for _, p := range []string{s.FramePath(gid), s.FrameDictPath(gid), s.FrameIndexPath(gid)} {
		fi, err := os.Stat(p)
		if err != nil {
			return 0, err
		}
		total += fi.Size()
	}
	return total, nil
}

func (s *Store) LoadManifest(name string) (*Manifest, error) {
	b, err := os.ReadFile(s.ManifestPath(name))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// store v1 kept manifests as plain JSON
		b, err = os.ReadFile(s.legacyManifestPath(name))
		if err != nil {
			return nil, err
		}
		var m1 Manifest
		if jerr := json.Unmarshal(b, &m1); jerr != nil {
			return nil, jerr
		}
		return &m1, nil
	}
	dec, err := DecodeAll(b)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", name, err)
	}
	var m Manifest
	if err := json.Unmarshal(dec, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// sortEntries orders files parents-first so materialize can mkdir as it goes.
func SortEntries(files []FileEntry) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}

// deleteOrphanBlobs removes blobs no longer referenced by the manifest.
// It refuses to run when other manifests exist unless force is set.
func (s *Store) DeleteOrphanBlobs(m *Manifest, force bool) (int, error) {
	if !force {
		ents, err := os.ReadDir(filepath.Join(s.Root, "manifests"))
		if err == nil && len(ents) > 1 {
			return 0, fmt.Errorf("store holds other manifests; pass --force to delete shared blobs")
		}
	}
	ref := map[string]bool{}
	if m.ImageConfig != "" {
		// The image config is what restore needs to rebuild a
		// docker-loadable archive; repack must never reclaim it.
		ref[m.ImageConfig] = true
	}
	for _, e := range m.Files {
		switch e.Kind {
		case KindFile:
			ref[e.Hash] = true
		case KindChunked:
			for _, c := range e.Chunks {
				if c.Frame == "" {
					ref[c.Hash] = true
				}
			}
		case KindDelta:
			ref[e.Base] = true
		}
	}
	n := 0
	_ = filepath.Walk(filepath.Join(s.Root, "blobs"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		hash := fi.Name()
		hash = hash[:len(hash)-4] // strip .zst
		if !ref[hash] {
			os.Remove(p)
			n++
		}
		return nil
	})
	return n, nil
}

var _ = bytes.MinRead
var _ = io.Discard
