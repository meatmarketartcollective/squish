package ops

// Autotune probes a sample of the store's blobs and computes the best
// pass assignment for this store: which files belong in the solid sweep
// window, which belong in standalone cold frames, and which versioned
// paths are worth delta frames. It predicts the final store size of
// every candidate pipeline from those probes and picks the smallest.
// Probing is read-only; nothing is written unless ApplyPlan runs.

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/aloysiusvonfck/squish/internal/bcj"
	"github.com/aloysiusvonfck/squish/internal/store"
)

// tuneOverheadBytes covers manifests, frame indexes and image configs,
// measured at ~3 MB on the 150-tree panel store.
const tuneOverheadBytes = 3_000_000

type TuneOpts struct {
	SampleBytes   int   // plain bytes probed per blob (default 128 KiB)
	ColdSamples   int   // ELF blobs probed for the cold verdict (default 8)
	WindowSamples int   // plain blobs probed for the window verdict (default 12)
	DeltaSamples  int   // versioned paths probed for the delta verdict (default 8)
	ColdMin       int64 // files under this never go cold (default 1 MiB)
	DeltaMin      int64 // mirrors delta --min-file (default 32 KiB)
	DeltaMax      int64 // mirrors delta --max-file (default 8 MiB)
}

type TuneCand struct {
	Name  string
	Bytes int64
}

type TunePlan struct {
	StoreBytes  int64
	UniqueFiles int
	UniqueBytes int64
	Winner      string
	Candidates  []TuneCand

	WindowFiles    int
	WindowBytes    int64
	ColdFiles      int
	ColdBytes      int64
	DeltaFiles     int // target blobs only; bases stay raw
	DeltaBytes     int64
	BaseFiles      int
	BaseBytes      int64
	VersionedPaths int

	WindowRate float64 // compressed/plain inside a shared window
	ColdRate   float64 // best standalone rate for ELFs
	DeltaRate  float64 // delta output / target size

	ColdHashes map[string]bool // apply: hashes to convert to cold frames
	DeltaPaths map[string]bool // apply: paths to delta
}

// Autotune inventories the store, probes blob prefixes and returns the
// computed plan. Read-only.
func Autotune(st *store.Store, manifests []*store.Manifest, o TuneOpts) (*TunePlan, error) {
	if o.SampleBytes <= 0 {
		o.SampleBytes = 128 << 10
	}
	if o.ColdSamples <= 0 {
		o.ColdSamples = 8
	}
	if o.WindowSamples <= 0 {
		o.WindowSamples = 12
	}
	if o.DeltaSamples <= 0 {
		o.DeltaSamples = 8
	}
	if o.ColdMin <= 0 {
		o.ColdMin = 1 << 20
	}
	if o.DeltaMin <= 0 {
		o.DeltaMin = 32 << 10
	}
	if o.DeltaMax <= 0 {
		o.DeltaMax = 8 << 20
	}

	// inventory: unique blobs, ELF filters from a 20-byte prefix, and
	// version groups per path (same window rules as DeltaPass)
	type blob struct {
		hash string
		size int64
		path string
		filt string
	}
	blobs := map[string]*blob{}
	type grp struct {
		counts map[string]int
	}
	groups := map[string]*grp{}
	for _, m := range manifests {
		for i := range m.Files {
			e := &m.Files[i]
			if e.Kind != store.KindFile {
				continue
			}
			b := blobs[e.Hash]
			if b == nil {
				// the ELF filter only matters for cold candidacy, which
				// requires size >= ColdMin, so skip prefix reads for the rest
				filt := ""
				if e.Size >= o.ColdMin {
					if p, err := st.ReadBlobPrefix(e.Hash, 20); err == nil {
						filt = elfFilter(p)
					}
				}
				b = &blob{e.Hash, e.Size, e.Path, filt}
				blobs[e.Hash] = b
			}
			if e.Size >= o.DeltaMin && e.Size <= o.DeltaMax {
				g := groups[e.Path]
				if g == nil {
					g = &grp{map[string]int{}}
					groups[e.Path] = g
				}
				g.counts[e.Hash]++
			}
		}
	}

	// delta classes: per group, base = most frequent hash (same tie-break
	// as DeltaPass); every other hash is a delta target. Bases must stay
	// raw for ReadDelta, so they can never go cold or into a frame.
	targets := map[string]bool{}
	bases := map[string]bool{}
	versioned := 0
	for _, g := range groups {
		if len(g.counts) < 2 {
			continue
		}
		versioned++
		base := ""
		for h, c := range g.counts {
			if base == "" || c > g.counts[base] || (c == g.counts[base] && h < base) {
				base = h
			}
		}
		bases[base] = true
		for h := range g.counts {
			if h != base {
				targets[h] = true
			}
		}
	}

	var coldList, winList []*blob
	var targetBytes, baseBytes, allBytes int64
	var coldCnt, winCnt, deltaCnt, baseCnt int
	for _, b := range blobs {
		allBytes += b.size
		switch {
		case targets[b.hash]:
			deltaCnt++
			targetBytes += b.size
		case bases[b.hash]:
			baseCnt++
			baseBytes += b.size
		case b.filt != "" && b.size >= o.ColdMin:
			coldCnt++
			coldList = append(coldList, b)
		default:
			winCnt++
			winList = append(winList, b)
		}
	}
	var coldBytes int64
	for _, b := range coldList {
		coldBytes += b.size
	}
	winBytes := allBytes - targetBytes - baseBytes - coldBytes

	// ---- probes -------------------------------------------------------
	type sample struct {
		b    *blob
		pfx  []byte
		marg int64
	}
	readPfx := func(h string) []byte {
		p, err := st.ReadBlobPrefix(h, int64(o.SampleBytes))
		if err != nil || len(p) < 16 {
			return nil
		}
		return p
	}
	var samples []sample
	sort.Slice(coldList, func(i, j int) bool { return coldList[i].size > coldList[j].size })
	for i, b := range coldList {
		if i >= o.ColdSamples {
			break
		}
		if p := readPfx(b.hash); p != nil {
			samples = append(samples, sample{b: b, pfx: p})
		}
	}
	sort.Slice(winList, func(i, j int) bool { return winList[i].hash < winList[j].hash })
	if n := len(winList); n > 0 {
		k := o.WindowSamples
		if k > n {
			k = n
		}
		for j := 0; j < k; j++ {
			b := winList[n*j/k]
			if p := readPfx(b.hash); p != nil {
				samples = append(samples, sample{b: b, pfx: p})
			}
		}
	}

	// standalone probe: best single-file compressor for this blob kind
	standalone := func(b *blob, pfx []byte) int64 {
		z, err := store.EncodeAllMax(pfx)
		if err != nil {
			return int64(len(pfx))
		}
		best := int64(len(z))
		switch b.filt {
		case "arm64":
			if c, err := store.EncodeAllMax(bcj.Encode(pfx)); err == nil && int64(len(c)) < best {
				best = int64(len(c))
			}
		case "x86":
			if store.XZHasFilter("x86") {
				if c, err := store.CompressCold("x86", pfx); err == nil && int64(len(c)) < best {
					best = int64(len(c))
				}
			}
		}
		return best
	}

	// window probe: leave-one-out marginals inside one shared stream
	var all []byte
	for _, s := range samples {
		all = append(all, s.pfx...)
	}
	full, err := store.EncodeAllMax(all)
	if err != nil {
		return nil, err
	}
	fullN := int64(len(full))
	var margN, rawN, coldRawN, coldStN int64
	for _, s := range samples {
		var rest []byte
		for _, t := range samples {
			if t.b.hash != s.b.hash {
				rest = append(rest, t.pfx...)
			}
		}
		c, err := store.EncodeAllMax(rest)
		if err != nil {
			return nil, err
		}
		s.marg = fullN - int64(len(c))
		margN += s.marg
		rawN += int64(len(s.pfx))
		if s.b.filt != "" {
			coldRawN += int64(len(s.pfx))
			coldStN += standalone(s.b, s.pfx)
		}
	}
	windowRate := float64(margN) / float64(rawN)
	coldRate := windowRate
	if coldRawN > 0 {
		coldRate = float64(coldStN) / float64(coldRawN)
	}

	// delta probe: CompressDelta on base/target prefixes of the biggest
	// versioned paths
	type dgrp struct {
		path   string
		base   string
		tgts   []string
		tbytes int64
	}
	var dgrps []*dgrp
	for path, g := range groups {
		if len(g.counts) < 2 {
			continue
		}
		base := ""
		for h, c := range g.counts {
			if base == "" || c > g.counts[base] || (c == g.counts[base] && h < base) {
				base = h
			}
		}
		d := &dgrp{path: path, base: base}
		for h := range g.counts {
			if h != base {
				d.tgts = append(d.tgts, h)
			}
		}
		for h, b := range blobs {
			if h == base {
				continue
			}
			for _, t := range d.tgts {
				if t == h {
					d.tbytes += b.size
				}
			}
		}
		dgrps = append(dgrps, d)
	}
	sort.Slice(dgrps, func(i, j int) bool { return dgrps[i].tbytes > dgrps[j].tbytes })
	var dOutN, dRawN int64
	for i, d := range dgrps {
		if i >= o.DeltaSamples {
			break
		}
		bp, tp := readPfx(d.base), readPfx(d.tgts[0])
		if bp == nil || tp == nil {
			continue
		}
		dd, err := store.CompressDelta(bp, tp)
		if err != nil {
			continue
		}
		dOutN += int64(len(dd))
		dRawN += int64(len(tp))
	}
	deltaRate := 1.0
	if dRawN > 0 {
		deltaRate = float64(dOutN) / float64(dRawN)
	}

	// ---- predictions ---------------------------------------------------
	oh := int64(tuneOverheadBytes)
	restBytes := winBytes
	cands := []TuneCand{
		{"sweep", allBytes*int64(windowRate*100+0.5)/100 + oh},
		{"delta+sweep", targetBytes*int64(deltaRate*100+0.5)/100 + baseBytes + restBytes*int64(windowRate*100+0.5)/100 + oh},
		{"cold+sweep", coldBytes*int64(coldRate*100+0.5)/100 + (restBytes+coldBytes)*int64(windowRate*100+0.5)/100 + oh - 0},
		{"cold+delta+sweep", targetBytes*int64(deltaRate*100+0.5)/100 + baseBytes + coldBytes*int64(coldRate*100+0.5)/100 + restBytes*int64(windowRate*100+0.5)/100 + oh},
	}
	// fix cold+sweep: everything except the cold class goes through the
	// window (targets+bases+plain all in the window, no delta pass)
	cands[2].Bytes = coldBytes*int64(coldRate*100+0.5)/100 + (allBytes-coldBytes)*int64(windowRate*100+0.5)/100 + oh

	winner := cands[0].Name
	winnerBytes := cands[0].Bytes
	for _, c := range cands {
		if c.Bytes < winnerBytes {
			winner, winnerBytes = c.Name, c.Bytes
		}
	}

	// apply sets
	coldHashes := map[string]bool{}
	for _, b := range coldList {
		coldHashes[b.hash] = true
	}
	deltaPaths := map[string]bool{}
	for _, d := range dgrps {
		deltaPaths[d.path] = true
	}

	sb, err := storeSize(st.Root)
	if err != nil {
		return nil, err
	}
	return &TunePlan{
		StoreBytes: sb, UniqueFiles: len(blobs), UniqueBytes: allBytes,
		Winner: winner, Candidates: cands,
		WindowFiles: winCnt, WindowBytes: winBytes,
		ColdFiles: coldCnt, ColdBytes: coldBytes,
		DeltaFiles: deltaCnt, DeltaBytes: targetBytes,
		BaseFiles: baseCnt, BaseBytes: baseBytes,
		VersionedPaths: versioned,
		WindowRate:     windowRate, ColdRate: coldRate, DeltaRate: deltaRate,
		ColdHashes: coldHashes, DeltaPaths: deltaPaths,
	}, nil
}

// ApplyPlan runs the winning pipeline: cold on the plan's ELF set, then
// delta on the plan's versioned paths, then a full sweep and gc.
func ApplyPlan(st *store.Store, manifests []*store.Manifest, plan *TunePlan, rollLimit int64, sortMode string) (int64, error) {
	if len(plan.ColdHashes) > 0 {
		for _, m := range manifests {
			if _, _, err := ColdPass(st, m, 1, plan.ColdHashes); err != nil {
				return 0, err
			}
		}
	}
	if len(plan.DeltaPaths) > 0 {
		if _, _, err := DeltaPass(st, manifests, 1, 1<<62, plan.DeltaPaths); err != nil {
			return 0, err
		}
	}
	if _, _, _, err := SweepStore(st, rollLimit, sortMode); err != nil {
		return 0, err
	}
	if _, _, err := GCAll(st); err != nil {
		return 0, err
	}
	return storeSize(st.Root)
}

func storeSize(root string) (int64, error) {
	var n int64
	err := filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n, err
}
