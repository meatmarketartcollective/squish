package ops

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/aloysiusvonfck/squish/internal/store"
)

// SweepStore re-compresses every file blob in the store into large
// stream frames: file contents are concatenated in the chosen order and
// each roll is compressed as ONE continuous zstd stream, sharing a
// single window the way a solid archive does. This is what per-blob
// compression cannot reach: files that are similar but not identical
// compress against each other instead of each against nothing.
//
// Identical content is stored once and every manifest referencing it
// points at the same span. Delta, cold, frame, chunked, special,
// symlink and elided entries are left untouched, as are image-config
// blobs. Reads decode the containing roll and slice the span; the store
// caches the last decoded roll, so sequential access over one roll
// decodes it once.
//
// sortMode picks the concatenation order:
//
//	"manifest": each manifest's files together, sorted by path. Best
//	read locality: one image's files live in consecutive rolls.
//	"path": all files across all manifests sorted by path (the solid
//	zstd experiment's order). Best ratio, scattered reads.
func SweepStore(st *store.Store, rollLimit int64, sortMode string) (manifests, files, frames int, err error) {
	if rollLimit <= 0 {
		rollLimit = 128 << 20
	}
	switch sortMode {
	case "manifest", "path":
	default:
		return 0, 0, 0, fmt.Errorf("sweep: unknown sort mode %q (manifest or path)", sortMode)
	}
	names, err := st.ListManifests()
	if err != nil {
		return 0, 0, 0, err
	}
	sort.Strings(names)
	ms := make([]*store.Manifest, 0, len(names))
	for _, n := range names {
		m, err := st.LoadManifest(n)
		if err != nil {
			return 0, 0, 0, err
		}
		ms = append(ms, m)
	}

	type spanRef struct {
		gid       string
		off, clen int64
	}
	swept := map[string]spanRef{}
	buf := &bytes.Buffer{}
	var idx []store.FrameFileEntry
	var rollRefs []string
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		gid := "w" + store.RandomID(8)
		comp, err := store.EncodeAllMax(buf.Bytes())
		if err != nil {
			return err
		}
		if err := st.WriteStreamFrame(gid, idx, comp); err != nil {
			return err
		}
		for i, h := range rollRefs {
			swept[h] = spanRef{gid, idx[i].Offset, idx[i].Clen}
		}
		frames++
		buf.Reset()
		idx = nil
		rollRefs = nil
		return nil
	}
	addBlob := func(h string) error {
		if _, done := swept[h]; done {
			return nil
		}
		raw, err := st.ReadBlob(h)
		if err != nil {
			return err
		}
		idx = append(idx, store.FrameFileEntry{Path: "s" + h, Offset: int64(buf.Len()), Clen: int64(len(raw)), Size: int64(len(raw))})
		buf.Write(raw)
		rollRefs = append(rollRefs, h)
		if int64(buf.Len()) >= rollLimit {
			return flush()
		}
		return nil
	}

	if sortMode == "path" {
		rep := map[string]string{} // hash -> smallest path seen
		for _, m := range ms {
			for _, e := range m.Files {
				if e.Kind != store.KindFile {
					continue
				}
				if old, ok := rep[e.Hash]; !ok || e.Path < old {
					rep[e.Hash] = e.Path
				}
			}
		}
		type hp struct {
			h, p string
		}
		list := make([]hp, 0, len(rep))
		for h, p := range rep {
			list = append(list, hp{h, p})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].p < list[j].p })
		for _, x := range list {
			if err := addBlob(x.h); err != nil {
				return 0, 0, 0, err
			}
		}
	} else {
		for _, m := range ms {
			var es []store.FileEntry
			for _, e := range m.Files {
				if e.Kind == store.KindFile {
					es = append(es, e)
				}
			}
			sort.Slice(es, func(i, j int) bool { return es[i].Path < es[j].Path })
			for _, e := range es {
				if err := addBlob(e.Hash); err != nil {
					return 0, 0, 0, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return 0, 0, 0, err
	}
	files = len(swept)

	for _, m := range ms {
		for i := range m.Files {
			e := &m.Files[i]
			if e.Kind != store.KindFile {
				continue
			}
			r, ok := swept[e.Hash]
			if !ok {
				return 0, 0, 0, fmt.Errorf("sweep: %s: file blob %s never stored", e.Path, e.Hash[:12])
			}
			e.Kind = store.KindFrame
			e.Frame = r.gid
			e.Offset = r.off
			e.Clen = r.clen
		}
		if err := st.SaveManifest(m); err != nil {
			return 0, 0, 0, err
		}
		manifests++
	}
	return manifests, files, frames, nil
}
