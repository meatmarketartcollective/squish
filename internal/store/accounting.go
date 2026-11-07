package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Accounting computes store-wide, dedup-aware storage numbers. One engine
// feeds stats, ls and anything else that reports sizes, so shared blobs are
// never counted twice and every per-image number answers the question the
// user actually has: what does removing this image free?

type ImageAccount struct {
	Name   string
	Raw    int64 // manifest RawBytes
	Elided int64 // regenerable content not stored at all
	Stored int64 // physical bytes this manifest references, each object once
	Unique int64 // bytes referenced only by this manifest: removing it frees this
	Shared int64 // bytes also referenced by another manifest
	OK     bool  // false when one of this manifest's objects is missing or unreadable
	Err    string
}

type StoreAccount struct {
	Images         []ImageAccount
	TotalRaw       int64
	TotalElided    int64
	Physical       int64 // every referenced object on disk, counted once
	UnresolvedKeys int   // referenced but unstat-able; excluded from Physical, never orphans
	OrphanBytes    int64 // on disk but referenced by nothing (gc candidates)
	OrphanCount    int
}

// objectKeys returns the physical store objects a manifest entry needs.
func objectKeys(e FileEntry) []string {
	switch e.Kind {
	case KindFile:
		return []string{"blob:" + e.Hash}
	case KindDelta:
		// the delta frame plus its base blob; the target's own blob is
		// gone once nothing references it directly
		return []string{"delta:" + e.Hash + "|" + e.Base, "blob:" + e.Base}
	case KindCold:
		return []string{"cold:" + e.Hash}
	case KindFrame:
		return []string{"frame:" + e.Frame}
	}
	return nil // dir, symlink, hardlink, elided, special store nothing
}

// objectSize stats one physical object.
func (s *Store) objectSize(key string) (int64, error) {
	switch {
	case strings.HasPrefix(key, "blob:"):
		return s.BlobSize(strings.TrimPrefix(key, "blob:"))
	case strings.HasPrefix(key, "delta:"):
		parts := strings.SplitN(strings.TrimPrefix(key, "delta:"), "|", 2)
		if len(parts) != 2 {
			return 0, fmt.Errorf("bad delta key %q", key)
		}
		return s.DeltaSize(parts[0], parts[1])
	case strings.HasPrefix(key, "cold:"):
		return s.ColdSize(strings.TrimPrefix(key, "cold:"))
	case strings.HasPrefix(key, "frame:"):
		gid := strings.TrimPrefix(key, "frame:")
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
	return 0, fmt.Errorf("bad object key %q", key)
}

// AccountStore walks every manifest and sizes the store honestly. Shared
// objects count once physically and show up as Shared on every image that
// references them; Unique is what removing that image frees. A missing or
// unreadable object is an error naming the manifest, never a silent zero:
// accounting that lies about a broken store is worse than none.
func (s *Store) AccountStore(ms []*Manifest) (*StoreAccount, error) {
	acc := &StoreAccount{}
	refcount := map[string]int{}
	sizes := map[string]int64{}
	resolve := func(key string) (int64, error) {
		if sz, ok := sizes[key]; ok {
			return sz, nil
		}
		sz, err := s.objectSize(key)
		if err != nil {
			return 0, err
		}
		sizes[key] = sz
		return sz, nil
	}
	acc.Images = make([]ImageAccount, len(ms))
	okCount := 0
	for i, m := range ms {
		row := &acc.Images[i]
		row.Name = m.Name
		row.Raw = m.RawBytes
		row.Elided = m.ElidedBytes
		row.OK = true
		// Every manifest's references are counted even when that manifest's
		// numbers end up unavailable: an object a broken manifest names is
		// never an orphan, and a blob shared with it still counts as shared
		// for the images that do resolve, so "removing frees" never
		// overstates.
		seen := map[string]bool{}
		for _, e := range m.Files {
			for _, key := range objectKeys(e) {
				if seen[key] {
					continue
				}
				seen[key] = true
				if _, err := resolve(key); err != nil {
					row.OK = false
					row.Err = fmt.Errorf("%s: %s: %w", m.Name, key, err).Error()
					continue
				}
				refcount[key]++
			}
		}
		if m.ImageConfig != "" {
			key := "blob:" + m.ImageConfig
			if !seen[key] {
				seen[key] = true
				if _, err := resolve(key); err != nil {
					row.OK = false
					row.Err = fmt.Errorf("%s: %s: %w", m.Name, key, err).Error()
				} else {
					refcount[key]++
				}
			}
		}
		if row.OK {
			okCount++
		}
	}
	for i := range acc.Images {
		row := &acc.Images[i]
		if !row.OK {
			m := ms[i]
			for _, e := range m.Files {
				for _, key := range objectKeys(e) {
					if _, ok := sizes[key]; !ok {
						acc.UnresolvedKeys++
					}
				}
			}
			continue
		}
		seen := map[string]bool{}
		add := func(key string) {
			if seen[key] {
				return
			}
			seen[key] = true
			sz := sizes[key]
			row.Stored += sz
			if refcount[key] == 1 {
				row.Unique += sz
			}
		}
		for _, e := range ms[i].Files {
			for _, key := range objectKeys(e) {
				add(key)
			}
		}
		if ms[i].ImageConfig != "" {
			add("blob:" + ms[i].ImageConfig)
		}
		row.Shared = row.Stored - row.Unique
		acc.TotalRaw += row.Raw
		acc.TotalElided += row.Elided
	}
	for _, sz := range sizes {
		acc.Physical += sz
	}
	s.scanOrphans(acc, refcount)
	if okCount == 0 {
		return acc, fmt.Errorf("no manifest accounted cleanly")
	}
	return acc, nil
}

func (s *Store) scanOrphans(acc *StoreAccount, refcount map[string]int) {
	consider := func(key string, size int64) {
		if refcount[key] == 0 {
			acc.OrphanBytes += size
			acc.OrphanCount++
		}
	}
	_ = filepath.Walk(filepath.Join(s.Root, "blobs"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		consider("blob:"+strings.TrimSuffix(fi.Name(), ".zst"), fi.Size())
		return nil
	})
	_ = filepath.Walk(filepath.Join(s.Root, "deltas"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		name := strings.TrimSuffix(fi.Name(), ".zst")
		i := strings.Index(name, ".from.")
		if i < 0 {
			consider("unknown:"+name, fi.Size())
			return nil
		}
		consider("delta:"+name[:i]+"|"+name[i+len(".from."):], fi.Size())
		return nil
	})
	_ = filepath.Walk(filepath.Join(s.Root, "cold"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		name := strings.TrimSuffix(strings.TrimSuffix(fi.Name(), ".xz"), ".zst")
		consider("cold:"+name, fi.Size())
		return nil
	})
	_ = filepath.Walk(filepath.Join(s.Root, "frames"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		name := fi.Name()
		var gid string
		switch {
		case strings.HasSuffix(name, ".idx.json"):
			gid = strings.TrimSuffix(name, ".idx.json")
		case strings.HasSuffix(name, ".dict"):
			gid = strings.TrimSuffix(name, ".dict")
		case strings.HasSuffix(name, ".zst"):
			gid = strings.TrimSuffix(name, ".zst")
		default:
			consider("unknown:"+name, fi.Size())
			return nil
		}
		consider("frame:"+gid, fi.Size())
		return nil
	})
}
