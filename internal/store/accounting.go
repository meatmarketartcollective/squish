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
}

type StoreAccount struct {
	Images      []ImageAccount
	TotalRaw    int64
	TotalElided int64
	Physical    int64 // every referenced object on disk, counted once
	OrphanBytes int64 // on disk but referenced by nothing (gc candidates)
	OrphanCount int
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
	touch := func(m *Manifest, key string, seen map[string]bool) error {
		if seen[key] {
			return nil
		}
		seen[key] = true
		if _, err := resolve(key); err != nil {
			return fmt.Errorf("%s: %s: %w", m.Name, key, err)
		}
		refcount[key]++
		return nil
	}
	for _, m := range ms {
		seen := map[string]bool{}
		for _, e := range m.Files {
			for _, key := range objectKeys(e) {
				if err := touch(m, key, seen); err != nil {
					return nil, err
				}
			}
		}
		if m.ImageConfig != "" {
			if err := touch(m, "blob:"+m.ImageConfig, seen); err != nil {
				return nil, err
			}
		}
	}
	for _, m := range ms {
		ia := ImageAccount{Name: m.Name, Raw: m.RawBytes, Elided: m.ElidedBytes}
		seen := map[string]bool{}
		add := func(key string) {
			if seen[key] {
				return
			}
			seen[key] = true
			sz := sizes[key]
			ia.Stored += sz
			if refcount[key] == 1 {
				ia.Unique += sz
			}
		}
		for _, e := range m.Files {
			for _, key := range objectKeys(e) {
				add(key)
			}
		}
		if m.ImageConfig != "" {
			add("blob:" + m.ImageConfig)
		}
		ia.Shared = ia.Stored - ia.Unique
		acc.Images = append(acc.Images, ia)
		acc.TotalRaw += ia.Raw
		acc.TotalElided += ia.Elided
	}
	for _, sz := range sizes {
		acc.Physical += sz
	}
	s.scanOrphans(acc, refcount)
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
