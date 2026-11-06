package bcj

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestRoundTripRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 50; trial++ {
		n := rng.Intn(4096)
		data := make([]byte, n)
		rng.Read(data)
		enc := Encode(data)
		dec := Decode(enc)
		if !bytes.Equal(dec, data) {
			t.Fatalf("trial %d: round trip differs (n=%d)", trial, n)
		}
	}
}

func TestRoundTripSyntheticBranches(t *testing.T) {
	// a stream of BL and ADRP instructions plus decoys
	buf := make([]byte, 0, 400*4)
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 100; i++ {
		var w uint32
		switch rng.Intn(4) {
		case 0: // BL
			w = 0x94000000 | (rng.Uint32() & 0x03FFFFFF)
		case 1: // ADRP
			w = 0x90000000 | (rng.Uint32() & 0x60FFFFFF)
		case 2: // B (left alone)
			w = 0x14000000 | (rng.Uint32() & 0x03FFFFFF)
		default:
			w = rng.Uint32()
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], w)
		buf = append(buf, b[:]...)
	}
	enc := Encode(buf)
	if bytes.Equal(enc, buf) {
		t.Fatal("filter changed nothing on branch-heavy input")
	}
	if !bytes.Equal(Decode(enc), buf) {
		t.Fatal("round trip differs on branch-heavy input")
	}
}

func TestTailBytesUntouched(t *testing.T) {
	data := []byte{0x00, 0x94, 0x00, 0x00, 0x00, 0x00, 0x00}
	enc := Encode(data)
	// the trailing 3 bytes must survive verbatim
	if !bytes.Equal(enc[4:], data[4:]) {
		t.Fatal("tail bytes were modified")
	}
	if !bytes.Equal(Decode(enc), data) {
		t.Fatal("round trip with tail differs")
	}
}

// TestBCJGain measures the BCJ pre-filter's effect on real binaries. It
// runs only when SQUISH_BENCH_FILE points at an executable.
func TestBCJGain(t *testing.T) {
	path := os.Getenv("SQUISH_BENCH_FILE")
	if path == "" {
		t.Skip("set SQUISH_BENCH_FILE to a binary to measure the BCJ gain")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	encRaw, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithWindowSize(1<<27))
	defer encRaw.Close()
	cRaw := encRaw.EncodeAll(raw, nil)
	filtered := Encode(raw)
	encFil, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithWindowSize(1<<27))
	defer encFil.Close()
	cFil := encFil.EncodeAll(filtered, nil)
	if !bytes.Equal(Decode(filtered), raw) {
		t.Fatal("round trip differs")
	}
	t.Logf("raw=%d zstd=%d bcj+zstd=%d win=%.1f%%", len(raw), len(cRaw), len(cFil), 100*(1-float64(len(cFil))/float64(len(cRaw))))
}
