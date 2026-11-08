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

	"github.com/aloysiusvonfck/squish/internal/store"
)

// Materialize reconstructs the image tree at out using symlink-resistant, directory-relative writes.
func Materialize(st *store.Store, m *store.Manifest, out string, useHardlinks, verifyEach bool) (int, error) {
	return materializeSecure(st, m, out, useHardlinks, verifyEach)
}

// readEntryData returns an entry's stored content; dir, symlink,
// hardlink, elided and special entries have none.
func readEntryData(st *store.Store, e store.FileEntry) ([]byte, error) {
	switch e.Kind {
	case store.KindFile:
		return st.ReadBlob(e.Hash)
	case store.KindFrame:
		return st.ReadFrameFile(e.Frame, e.Offset, e.Clen)
	case store.KindDelta:
		return st.ReadDelta(e.Hash, e.Base)
	case store.KindCold:
		return st.ReadColdEntry(&e)
	case store.KindChunked:
		return st.ReadChunked(&e)
	}
	return nil, fmt.Errorf("%s: kind %q has no stored content", e.Path, e.Kind)
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
	// verify is order-independent, so frame entries are read grouped
	// by frame and offset: that turns the store's roll cache into
	// one decode per roll instead of one per straddling file
	idxs := make([]int, len(m.Files))
	for i := range idxs {
		idxs[i] = i
	}
	sort.SliceStable(idxs, func(a, b int) bool {
		fa, fb := m.Files[idxs[a]].Frame, m.Files[idxs[b]].Frame
		if fa != fb {
			return fa < fb
		}
		return m.Files[idxs[a]].Offset < m.Files[idxs[b]].Offset
	})
	for _, i := range idxs {
		e := m.Files[i]
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
		case store.KindDelta:
			data, err := st.ReadDelta(e.Hash, e.Base)
			if err != nil {
				return checked, bad, fmt.Errorf("%s: %w", e.Path, err)
			}
			checked++
			if int64(len(data)) != e.Size || store.HashBytes(data) != e.Hash {
				bad++
			}
		case store.KindCold:
			data, err := st.ReadColdEntry(&e)
			if err != nil {
				return checked, bad, fmt.Errorf("%s: %w", e.Path, err)
			}
			checked++
			if int64(len(data)) != e.Size || store.HashBytes(data) != e.Hash {
				bad++
			}
		case store.KindChunked:
			data, err := st.ReadChunked(&e)
			if err != nil {
				return checked, bad, fmt.Errorf("%s: %w", e.Path, err)
			}
			checked++
			if int64(len(data)) != e.Size || store.HashBytes(data) != e.Hash {
				bad++
			}
		}
	}
	if m.ImageConfig != "" {
		data, err := st.ReadBlob(m.ImageConfig)
		if err != nil {
			return checked, bad, fmt.Errorf("image config: %w", err)
		}
		checked++
		if store.HashBytes(data) != m.ImageConfig {
			bad++
		}
	}
	return checked, bad, nil
}

// Stats summarizes a manifest against the store.
func Stats(st *store.Store, m *store.Manifest) error {
	blobBytes := map[string]int64{}
	frameDone := map[string]bool{}
	var stored int64
	addFrame := func(gid string) error {
		if frameDone[gid] {
			return nil
		}
		sz, err := st.FrameBytes(gid)
		if err != nil {
			return err
		}
		stored += sz
		frameDone[gid] = true
		return nil
	}
	addBlob := func(hash string) error {
		if _, ok := blobBytes[hash]; ok {
			return nil
		}
		sz, err := st.BlobSize(hash)
		if err != nil {
			return err
		}
		blobBytes[hash] = sz
		stored += sz
		return nil
	}
	for _, e := range m.Files {
		switch e.Kind {
		case store.KindFile:
			if err := addBlob(e.Hash); err != nil {
				return err
			}
		case store.KindFrame:
			if err := addFrame(e.Frame); err != nil {
				return err
			}
		case store.KindChunked:
			for _, c := range e.Chunks {
				if c.Frame != "" {
					if err := addFrame(c.Frame); err != nil {
						return err
					}
				} else if err := addBlob(c.Hash); err != nil {
					return err
				}
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
	if m.ChunkDedupBytes > 0 {
		fmt.Printf("chunk dedup     %d B\n", m.ChunkDedupBytes)
	}
	if comp := m.RawBytes - m.ElidedBytes - m.DedupBytes - m.ChunkDedupBytes - stored; comp > 0 {
		fmt.Printf("compression     %d B\n", comp)
	}
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
	// chunk frames: every chunked entry's not-yet-framed chunks go
	// into per-manifest chunk frames, each unique chunk stored once
	// with the dictionary benefit. Chunk refs are rewritten to point
	// into the frame; cross-manifest shared chunks keep their blobs.
	var chunkEntries []*store.FileEntry
	for i := range m.Files {
		if m.Files[i].Kind == store.KindChunked {
			chunkEntries = append(chunkEntries, &m.Files[i])
		}
	}
	if len(chunkEntries) > 0 {
		refsByHash := map[string][]*store.ChunkRef{}
		uniq := map[string]bool{}
		var order []string
		for _, e := range chunkEntries {
			for i := range e.Chunks {
				c := &e.Chunks[i]
				if c.Frame != "" {
					continue
				}
				if !uniq[c.Hash] {
					uniq[c.Hash] = true
					order = append(order, c.Hash)
				}
				refsByHash[c.Hash] = append(refsByHash[c.Hash], c)
			}
		}
		sort.Strings(order)
		if len(order) > 0 {
			dict, err := buildChunkDict(st, order, uniq)
			if err != nil {
				return groups, moved, err
			}
			const rollLimit = 96 << 20
			var payload []byte
			var idx []store.FrameFileEntry
			var rollRefs []string
			flush := func() error {
				if len(payload) == 0 {
					return nil
				}
				gid := "c" + store.RandomID(8)
				if err := st.WriteFrame(gid, dict, idx, payload); err != nil {
					return err
				}
				for _, h := range rollRefs {
					for _, c := range refsByHash[h] {
						c.Frame = gid
						c.Offset = offsetOf(idx, "c"+h)
						c.Clen = clenOf(idx, "c"+h)
					}
				}
				groups++
				payload, idx, rollRefs = nil, nil, nil
				return nil
			}
			for _, h := range order {
				raw, err := st.ReadBlob(h)
				if err != nil {
					return groups, moved, err
				}
				comp, err := encodeWithDict(raw, dict)
				if err != nil {
					return groups, moved, err
				}
				idx = append(idx, store.FrameFileEntry{Path: "c" + h, Offset: int64(len(payload)), Clen: int64(len(comp)), Size: int64(len(raw))})
				payload = append(payload, comp...)
				rollRefs = append(rollRefs, h)
				if len(payload) >= rollLimit {
					if err := flush(); err != nil {
						return groups, moved, err
					}
				}
			}
			if err := flush(); err != nil {
				return groups, moved, err
			}
			moved += len(chunkEntries)
		}
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

// buildChunkDict builds a frame dictionary from a sample of the group's
// largest unique chunks: context shared across the batch without a trainer.
func buildChunkDict(st *store.Store, hashes []string, uniq map[string]bool) ([]byte, error) {
	type cs struct {
		h string
		n int64
	}
	var sized []cs
	for _, h := range hashes {
		if !uniq[h] {
			continue
		}
		sz, err := st.BlobSize(h)
		if err != nil {
			return nil, err
		}
		sized = append(sized, cs{h, sz})
	}
	sort.Slice(sized, func(i, j int) bool { return sized[i].n > sized[j].n })
	var dict []byte
	for i := 0; i < len(sized) && i < 8 && len(dict) < 96*1024; i++ {
		raw, err := st.ReadBlob(sized[i].h)
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

// safeJoin joins a manifest path under out, refusing anything that
// would escape the output directory.
func safeJoin(out, path string) (string, error) {
	if path == "" {
		return out, nil
	}
	rel := filepath.Clean(filepath.FromSlash(path))
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: path escapes the output directory", path)
	}
	return filepath.Join(out, rel), nil
}

// ensureInsideOut refuses a directory whose real location (following any
// symlinks) sits outside out, so writes can never escape through a
// symlinked parent the layer itself created.
func ensureInsideOut(out, dir string) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	outRes, err := filepath.EvalSymlinks(out)
	if err != nil {
		return err
	}
	outRes, _ = filepath.Abs(outRes)
	resolved, _ = filepath.Abs(resolved)
	if resolved != outRes && !strings.HasPrefix(resolved, outRes+string(filepath.Separator)) {
		return fmt.Errorf("%s: resolves outside the output directory", dir)
	}
	return nil
}
