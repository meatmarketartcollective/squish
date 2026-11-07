package ops

// Advanced passes: cross-image version deltas, BCJ cold frames for large
// read-once binaries, and whole-store garbage collection.

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aloysiusvonfck/squish/internal/store"
)

// DeltaPass finds files that exist at the same path across manifests but
// with different content (e.g. two versions of the same library) and
// replaces the larger versions with delta frames against a shared base.
// Only plain-file entries in the size window are considered.
// Returns how many entries were converted and the estimated bytes saved
// (only for blobs that become unreferenced, so it is what gc will free).
func DeltaPass(st *store.Store, manifests []*store.Manifest, minFile, maxFile int64) (converted int, estSaved int64, err error) {
	type entryRef struct {
		m    *store.Manifest
		idx  int
		hash string
	}
	byPath := map[string][]entryRef{}
	for _, m := range manifests {
		for i, e := range m.Files {
			if e.Kind != store.KindFile || e.Size < minFile || e.Size > maxFile {
				continue
			}
			byPath[e.Path] = append(byPath[e.Path], entryRef{m, i, e.Hash})
		}
	}
	// target hash -> delta size, for savings accounting after conversion
	convertedSizes := map[string]int64{}
	for _, refs := range byPath {
		counts := map[string]int{}
		for _, r := range refs {
			counts[r.hash]++
		}
		if len(counts) < 2 {
			continue
		}
		// base: most frequent hash; ties break to the lexicographically
		// smallest so every run picks the same base
		base := ""
		for h := range counts {
			if base == "" || counts[h] > counts[base] || (counts[h] == counts[base] && h < base) {
				base = h
			}
		}
		for h := range counts {
			if h == base {
				continue
			}
			// skip if this target already has a delta against this base
			if _, ok := convertedSizes[h]; ok {
				continue
			}
			data, err := st.ReadBlob(h)
			if err != nil {
				return converted, estSaved, fmt.Errorf("delta read %s: %w", h[:12], err)
			}
			dsz, err := st.PutDelta(h, base, data)
			if err != nil {
				return converted, estSaved, err
			}
			bsz, err := st.BlobSize(h)
			if err != nil {
				return converted, estSaved, err
			}
			if dsz >= bsz {
				// delta is not smaller: remove the frame, keep the blob
				_ = os.Remove(st.DeltaPath(h, base))
				continue
			}
			// convert every entry in this path group that uses this hash
			for _, r := range refs {
				e := &r.m.Files[r.idx]
				if e.Kind == store.KindFile && e.Hash == h {
					e.Kind = store.KindDelta
					e.Base = base
					e.Clen = dsz
					converted++
				}
			}
			convertedSizes[h] = dsz
		}
	}
	// save every manifest we touched, then estimate savings from blobs
	// that no manifest references anymore
	for _, m := range manifests {
		if err := st.SaveManifest(m); err != nil {
			return converted, estSaved, err
		}
	}
	if len(convertedSizes) > 0 {
		ref := referencedHashes(manifests)
		for h, dsz := range convertedSizes {
			if !ref[h] {
				bsz, err := st.BlobSize(h)
				if err != nil {
					continue
				}
				estSaved += bsz - dsz
			}
		}
	}
	return converted, estSaved, nil
}

// referencedHashes is the set of blob hashes the manifests still need:
// plain files and delta bases (cold frames do not need their source blob).
func referencedHashes(manifests []*store.Manifest) map[string]bool {
	ref := map[string]bool{}
	for _, m := range manifests {
		if m.ImageConfig != "" {
			ref[m.ImageConfig] = true
		}
		for _, e := range m.Files {
			switch e.Kind {
			case store.KindFile:
				ref[e.Hash] = true
			case store.KindChunked:
				for _, c := range e.Chunks {
					if c.Frame == "" {
						ref[c.Hash] = true
					}
				}
			case store.KindDelta:
				ref[e.Base] = true
			}
		}
	}
	return ref
}

// elfFilter reads an ELF header and returns the BCJ filter matching the
// binary's own architecture, or "" when the file is not an ELF we filter.
func elfFilter(data []byte) string {
	if len(data) < 20 || string(data[:4]) != "\x7fELF" {
		return ""
	}
	mach := int(data[18]) | int(data[19])<<8 // e_machine, little endian
	switch mach {
	case 183: // EM_AARCH64
		return "arm64"
	case 62: // EM_X86_64
		return "x86"
	default:
		return ""
	}
}

// ColdPass converts large files in one manifest to xz/BCJ cold frames
// meant for binaries that are read once per image, never per run.
// Files must be at least minSize bytes. Returns converted entries and
// estimated bytes saved.
func ColdPass(st *store.Store, m *store.Manifest, minSize int64) (converted int, estSaved int64, err error) {
	type res struct {
		filter string
		size   int64
	}
	done := map[string]res{}
	for i := range m.Files {
		e := &m.Files[i]
		if e.Kind != store.KindFile || e.Size < minSize {
			continue
		}
		if r, ok := done[e.Hash]; ok {
			e.Kind = store.KindCold
			e.Filter = r.filter
			e.Clen = r.size
			converted++
			continue
		}
		data, err := st.ReadBlob(e.Hash)
		if err != nil {
			return converted, estSaved, err
		}
		filter := elfFilter(data)
		if filter == "" {
			continue
		}
		// reuse an existing frame for the same hash
		if csz, statErr := st.ColdSize(e.Hash); statErr == nil {
			nf := filter
			if _, xzErr := os.Stat(st.ColdNativePath(e.Hash)); xzErr == nil {
				nf = "bcj-arm64"
			}
			e.Kind = store.KindCold
			e.Filter = nf
			e.Clen = csz
			done[e.Hash] = res{nf, csz}
			converted++
			continue
		}
		if filter == "arm64" {
			// native path: Go BCJ filter + zstd, no external tools
			csz, err := st.PutColdNative(e.Hash, data)
			if err != nil {
				return converted, estSaved, err
			}
			if csz >= int64(len(data)) {
				_ = os.Remove(st.ColdNativePath(e.Hash))
				done[e.Hash] = res{filter, int64(len(data))} // no win; skip
				continue
			}
			e.Kind = store.KindCold
			e.Filter = "bcj-arm64"
			e.Clen = csz
			done[e.Hash] = res{"bcj-arm64", csz}
			converted++
			continue
		}
		// x86 ELFs still go through an xz with BCJ support
		comp, err := store.CompressCold(filter, data)
		if err != nil {
			return converted, estSaved, err
		}
		if int64(len(comp)) >= int64(len(data)) {
			done[e.Hash] = res{filter, int64(len(data))} // no win; skip
			continue
		}
		if err := st.WriteCold(e.Hash, comp); err != nil {
			return converted, estSaved, err
		}
		e.Kind = store.KindCold
		e.Filter = filter
		e.Clen = int64(len(comp))
		done[e.Hash] = res{filter, int64(len(comp))}
		converted++
	}
	if converted > 0 {
		if err := st.SaveManifest(m); err != nil {
			return converted, estSaved, err
		}
	}
	if len(done) > 0 {
		ref := referencedHashes([]*store.Manifest{m})
		for h, r := range done {
			if r.size <= 0 || ref[h] {
				continue
			}
			bsz, err := st.BlobSize(h)
			if err != nil {
				continue
			}
			estSaved += bsz - r.size
		}
	}
	return converted, estSaved, nil
}

// GCAll removes every blob, delta frame and cold frame no manifest
// references. Delta bases are protected; delta targets and cold frames
// are only needed when a manifest still lists them.
func GCAll(st *store.Store) (removed int, reclaimed int64, err error) {
	names, err := st.ListManifests()
	if err != nil {
		return 0, 0, err
	}
	manifests := make([]*store.Manifest, 0, len(names))
	for _, n := range names {
		m, err := st.LoadManifest(n)
		if err != nil {
			return removed, reclaimed, err
		}
		manifests = append(manifests, m)
	}
	ref := referencedHashes(manifests)
	// which delta and cold files live in the manifest
	wantDelta := map[string]bool{} // "target.from.base"
	for _, m := range manifests {
		for _, e := range m.Files {
			switch e.Kind {
			case store.KindDelta:
				wantDelta[e.Hash+".from."+e.Base] = true
			case store.KindCold:
				// cold frames are keyed by hash; keep during blob walk
			}
		}
	}
	// blobs
	_ = filepath.Walk(filepath.Join(st.Root, "blobs"), func(p string, fi os.FileInfo, werr error) error {
		if werr != nil || fi.IsDir() || !strings.HasSuffix(p, ".zst") {
			return nil
		}
		h := strings.TrimSuffix(filepath.Base(p), ".zst")
		if _, e := hex.DecodeString(h); e != nil {
			return nil
		}
		if !ref[h] {
			if rmErr := os.Remove(p); rmErr == nil {
				removed++
				reclaimed += fi.Size()
			}
		}
		return nil
	})
	// deltas
	_ = filepath.Walk(filepath.Join(st.Root, "deltas"), func(p string, fi os.FileInfo, werr error) error {
		if werr != nil || fi.IsDir() {
			return nil
		}
		n := strings.TrimSuffix(filepath.Base(p), ".zst")
		parts := strings.SplitN(n, ".from.", 2)
		if len(parts) != 2 {
			return nil
		}
		if _, e := hex.DecodeString(parts[0]); e != nil {
			return nil
		}
		if _, e := hex.DecodeString(parts[1]); e != nil {
			return nil
		}
		if !wantDelta[n] {
			if rmErr := os.Remove(p); rmErr == nil {
				removed++
				reclaimed += fi.Size()
			}
		}
		return nil
	})
	// cold frames (native .zst and xz .xz)
	_ = filepath.Walk(filepath.Join(st.Root, "cold"), func(p string, fi os.FileInfo, werr error) error {
		if werr != nil || fi.IsDir() {
			return nil
		}
		base := filepath.Base(p)
		var h string
		switch {
		case strings.HasSuffix(base, ".zst"):
			h = strings.TrimSuffix(base, ".zst")
		case strings.HasSuffix(base, ".xz"):
			h = strings.TrimSuffix(base, ".xz")
		default:
			return nil
		}
		if _, e := hex.DecodeString(h); e != nil {
			return nil
		}
		if _, e := hex.DecodeString(h); e != nil {
			return nil
		}
		if !ref[h] && !coldWanted(manifests, h) {
			if rmErr := os.Remove(p); rmErr == nil {
				removed++
				reclaimed += fi.Size()
			}
		}
		return nil
	})
	return removed, reclaimed, nil
}

func coldWanted(manifests []*store.Manifest, h string) bool {
	for _, m := range manifests {
		for _, e := range m.Files {
			if e.Kind == store.KindCold && e.Hash == h {
				return true
			}
		}
	}
	return false
}

// SortManifestNames keeps output stable when callers print a set.
func SortManifestNames(names []string) []string {
	sort.Strings(names)
	return names
}
