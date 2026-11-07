package main

// Gear-based content-defined chunking (FastCDC, Xia et al. 2016) plus
// the deduplication accounting E1 reports. Deterministic across builds:
// the Gear table is generated from a fixed seed at init time.

import (
	"crypto/sha256"
	"sync"
)

var gearTable [256]uint64

func init() {
	var x uint64 = 0x9E3779B97F4A7C15
	for i := 0; i < 256; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		gearTable[i] = x
	}
}

// Chunk is one content-defined chunk with its content hash.
type Chunk struct {
	Hash [sha256.Size]byte
	Size int
}

// Scanner turns a byte stream into content-defined chunks whose
// boundaries are stable under insertions and deletions that occur
// before the cut point. Feed the stream, then call Finish and read
// Chunks. Memory use is bounded by the maximum chunk size.
type Scanner struct {
	min, avg, max int
	maskS, maskL  uint64

	h        uint64
	buf      []byte
	chunkLen int
	started  bool
	Chunks   []Chunk
}

// NewScanner builds a scanner. Requires 0 < min < avg < max.
func NewScanner(min, avg, max int) *Scanner {
	if min <= 0 || avg <= min || max <= avg {
		panic("cdcbench: need 0 < min < avg < max")
	}
	bits := 0
	for 1<<bits < avg {
		bits++
	}
	maskS := (uint64(1) << (bits - 1)) - 1
	maskL := (uint64(1) << (bits + 1)) - 1
	return &Scanner{min: min, avg: avg, max: max, maskS: maskS, maskL: maskL}
}

// Feed consumes p. Chunks completed by p are appended to Chunks.
func (s *Scanner) Feed(p []byte) {
	if s.buf == nil {
		s.buf = make([]byte, 0, s.max)
	}
	for _, b := range p {
		s.h = (s.h << 8) + gearTable[b&0xFF]
		s.buf = append(s.buf, b)
		s.chunkLen++
		if !s.started {
			s.started = true
			continue
		}
		if s.chunkLen >= s.min &&
			(s.chunkLen >= s.max ||
				(s.chunkLen < s.avg && s.h&s.maskS == 0) ||
				(s.chunkLen >= s.avg && s.h&s.maskL == 0)) {
			s.emit()
		}
	}
}

// Finish flushes the trailing partial chunk, if any.
func (s *Scanner) Finish() {
	if s.started {
		s.emit()
	}
}

func (s *Scanner) emit() {
	sum := sha256.Sum256(s.buf)
	s.Chunks = append(s.Chunks, Chunk{Hash: sum, Size: s.chunkLen})
	s.buf = s.buf[:0]
	s.chunkLen = 0
	s.h = 0
	s.started = false
}

// Accountor records whole-file and chunk-level deduplication results.
// Safe for concurrent use.
type Accountor struct {
	mu          sync.Mutex
	fileSeen    map[[sha256.Size]byte]struct{}
	chunkSeen   map[[sha256.Size]byte]struct{}
	uniqueFiles int64
	uniqueFileB int64
	dupFileB    int64
	uniqueChB   int64
	dupChB      int64
	chunks      int64
	hist        map[int]int64
}

func newAccountor() *Accountor {
	return &Accountor{
		fileSeen:  map[[sha256.Size]byte]struct{}{},
		chunkSeen: map[[sha256.Size]byte]struct{}{},
		hist:      map[int]int64{},
	}
}

// AddFile records one whole file by hash and size.
func (a *Accountor) AddFile(sum [sha256.Size]byte, size int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.fileSeen[sum]; ok {
		a.dupFileB += size
		return
	}
	a.fileSeen[sum] = struct{}{}
	a.uniqueFiles++
	a.uniqueFileB += size
}

// AddChunk records one chunk.
func (a *Accountor) AddChunk(c Chunk) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.chunks++
	a.hist[bucket(c.Size)]++
	if _, ok := a.chunkSeen[c.Hash]; ok {
		a.dupChB += int64(c.Size)
		return
	}
	a.chunkSeen[c.Hash] = struct{}{}
	a.uniqueChB += int64(c.Size)
}

// Snapshot returns the aggregate result.
func (a *Accountor) Snapshot() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	h := make(map[int]int64, len(a.hist))
	for k, v := range a.hist {
		h[k] = v
	}
	return Stats{
		UniqueFiles:  a.uniqueFiles,
		UniqueFileB:  a.uniqueFileB,
		DupFileB:     a.dupFileB,
		Chunks:       a.chunks,
		UniqueChunkB: a.uniqueChB,
		DupChunkB:    a.dupChB,
		Hist:         h,
	}
}

// Stats is the aggregate result of a bench run.
type Stats struct {
	Files        int64         `json:"files"`
	TotalBytes   int64         `json:"total_bytes"`
	UniqueFiles  int64         `json:"unique_files"`
	UniqueFileB  int64         `json:"unique_file_bytes"`
	DupFileB     int64         `json:"duplicate_file_bytes"`
	Chunks       int64         `json:"chunks"`
	UniqueChunkB int64         `json:"unique_chunk_bytes"`
	DupChunkB    int64         `json:"duplicate_chunk_bytes"`
	ChunkMin     int           `json:"chunk_min"`
	ChunkAvg     int           `json:"chunk_avg"`
	ChunkMax     int           `json:"chunk_max"`
	Dirs         []string      `json:"dirs"`
	Hist         map[int]int64 `json:"histogram,omitempty"`
}

func bucket(n int) int {
	b := 0
	for 1<<b < n {
		b++
	}
	return b
}
