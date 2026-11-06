// Package store implements Stackfold's per-file content-addressed store.
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

	"golang.org/x/crypto/blake2b"
	"github.com/klauspost/compress/zstd"
)

const StoreVersion = 1

// Kinds for manifest entries.
const (
	KindDir     = "dir"
	KindFile    = "file" // unique blob in the store
	KindFrame   = "frame"
	KindSymlink = "symlink"
	KindLink    = "hardlink"
	KindElided  = "elided"
	KindSpecial = "special"
)

type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	Kind   string `json:"kind"`
	Hash   string `json:"hash,omitempty"`
	Link   string `json:"link,omitempty"`
	Frame  string `json:"frame,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	Clen   int64  `json:"clen,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type Manifest struct {
	Version     int          `json:"version"`
	Name        string       `json:"name"`
	Created     time.Time    `json:"created"`
	Source      string       `json:"source"`
	Layers      []string     `json:"layers,omitempty"`
	Files       []FileEntry  `json:"files"`
	RawBytes    int64        `json:"raw_bytes"`
	DedupBytes  int64        `json:"dedup_bytes"`
	ElidedBytes int64        `json:"elided_bytes"`
	StoredBytes int64        `json:"stored_bytes"`
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
	return filepath.Join(s.Root, "frames", gid+".idx.json")
}
func (s *Store) ManifestPath(name string) string {
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

// EncodeAllFast compresses data at the install-path setting.
func EncodeAllFast(data []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, encOpts(zstd.SpeedDefault)...)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil), nil
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
	comp, err := EncodeAllFast(data)
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
	return os.WriteFile(s.FrameIndexPath(gid), b, 0o644)
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

// SaveManifest writes the manifest JSON atomically and computes StoredBytes.
func (s *Store) SaveManifest(m *Manifest) error {
	seenBlob := map[string]bool{}
	seenFrame := map[string]bool{}
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
		case KindFrame:
			if !seenFrame[e.Frame] {
				seenFrame[e.Frame] = true
				for _, p := range []string{s.FramePath(e.Frame), s.FrameDictPath(e.Frame), s.FrameIndexPath(e.Frame)} {
					fi, err := os.Stat(p)
					if err != nil {
						return err
					}
					stored += fi.Size()
				}
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
	tmp := s.ManifestPath(m.Name) + ".tmp" + RandomID(4)
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.ManifestPath(m.Name))
}

func (s *Store) LoadManifest(name string) (*Manifest, error) {
	b, err := os.ReadFile(s.ManifestPath(name))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
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
	for _, e := range m.Files {
		if e.Kind == KindFile {
			ref[e.Hash] = true
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
