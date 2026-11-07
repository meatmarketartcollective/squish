package main

import (
	"math/rand"
	"testing"
)

func boundaries(sc *Scanner) []int {
	var out []int
	pos := 0
	for _, c := range sc.Chunks {
		pos += c.Size
		out = append(out, pos)
	}
	return out
}

func scan(t *testing.T, min, avg, max int, data []byte) *Scanner {
	t.Helper()
	sc := NewScanner(min, avg, max)
	sc.Feed(data)
	sc.Finish()
	return sc
}

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

// Boundaries strictly before a modification are unchanged.
func TestStabilityBeforeInsertion(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	base := randBytes(rng, 512<<10)
	b1 := boundaries(scan(t, 2048, 8192, 32768, base))

	const cutAt = 100 * 1024
	ins := randBytes(rng, 97)
	mutated := append(append([]byte{}, base[:cutAt]...), append(ins, base[cutAt:]...)...)
	b2 := boundaries(scan(t, 2048, 8192, 32768, mutated))

	if len(b2) == 0 || len(b1) == 0 {
		t.Fatal("no boundaries produced")
	}
	n := 0
	for _, b := range b1 {
		if b >= cutAt {
			break
		}
		if n >= len(b2) || b2[n] != b {
			t.Fatalf("boundary %d moved: was %d, now %v", n, b, b2[n])
		}
		n++
	}
	if n == 0 {
		t.Fatal("no pre-insertion boundaries to check; test is vacuous")
	}
}

// Shared content is found across shifted files: some chunk inside a
// byte-identical region matches between two files where the region sits
// at different offsets. Gear cut points are content-determined but
// phase-dependent after a change, so alignment is partial, not exact;
// this asserts the dedup signal exists at all.
func TestSharedRegionSomeChunksMatch(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	shared := randBytes(rng, 64<<10)
	a := append(append(randBytes(rng, 16<<10), shared...), randBytes(rng, 16<<10)...)
	b := append(append(randBytes(rng, 24<<10), shared...), randBytes(rng, 8<<10)...)

	chunksA := scan(t, 2048, 8192, 32768, a).Chunks
	chunksB := scan(t, 2048, 8192, 32768, b).Chunks
	seenB := map[[32]byte]bool{}
	for _, c := range chunksB {
		seenB[c.Hash] = true
	}
	matched := 0
	for _, c := range chunksA {
		if seenB[c.Hash] {
			matched++
		}
	}
	t.Logf("matching chunks across shifted files: %d of %d", matched, len(chunksA))
	if matched < 1 {
		t.Fatalf("no chunk of the shared region matched across shifted files")
	}
}

func TestChunkSizesWithinBounds(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	data := randBytes(rng, 1<<20)
	sc := scan(t, 2048, 8192, 32768, data)
	total := 0
	for i, c := range sc.Chunks {
		if c.Size <= 0 {
			t.Fatalf("chunk %d empty", i)
		}
		if c.Size > 32768 {
			t.Fatalf("chunk %d over max: %d", i, c.Size)
		}
		if i < len(sc.Chunks)-1 && c.Size < 2048 {
			t.Fatalf("chunk %d under min: %d", i, c.Size)
		}
		total += c.Size
	}
	if total != len(data) {
		t.Fatalf("coverage %d != %d", total, len(data))
	}
}

func TestTinyInputIsOneChunk(t *testing.T) {
	sc := scan(t, 2048, 8192, 32768, []byte("hello world"))
	if len(sc.Chunks) != 1 || sc.Chunks[0].Size != 11 {
		t.Fatalf("want one 11-byte chunk, got %+v", sc.Chunks)
	}
}

func TestEmptyStream(t *testing.T) {
	sc := scan(t, 2048, 8192, 32768, nil)
	if len(sc.Chunks) != 0 {
		t.Fatalf("empty input produced chunks: %+v", sc.Chunks)
	}
}

func TestAccountorDedup(t *testing.T) {
	acc := newAccountor()
	var sum [32]byte
	copy(sum[:], "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	acc.AddFile(sum, 1000)
	acc.AddFile(sum, 1000)
	var c1, c2 Chunk
	copy(c1.Hash[:], "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	copy(c2.Hash[:], "cccccccccccccccccccccccccccccccc")
	c1.Size, c2.Size = 8192, 4096
	acc.AddChunk(c1)
	acc.AddChunk(c2)
	acc.AddChunk(c1)
	st := acc.Snapshot()
	if st.UniqueFiles != 1 || st.DupFileB != 1000 {
		t.Fatalf("file accounting wrong: %+v", st)
	}
	if st.Chunks != 3 || st.UniqueChunkB != 8192+4096 || st.DupChunkB != 8192 {
		t.Fatalf("chunk accounting wrong: %+v", st)
	}
}
