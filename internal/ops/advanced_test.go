package ops

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aloysiusvonfck/squish/internal/store"
)

func newManifest(t *testing.T, st *store.Store, name string, files map[string][]byte) *store.Manifest {
	t.Helper()
	m := &store.Manifest{Name: name, Source: "test"}
	for p, data := range files {
		h, err := st.PutBlob(data)
		if err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, store.FileEntry{Path: p, Size: int64(len(data)), Mode: 0o644, Kind: store.KindFile, Hash: h})
	}
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func variant(base []byte, seed byte) []byte {
	out := append([]byte(nil), base...)
	for i := 0; i < len(out); i += 512 {
		out[i] ^= seed
	}
	return out
}

func TestDeltaPassRoundTrip(t *testing.T) {
	if os.Getenv("SQUISH_XZ") == "" {
		if _, err := os.Stat("/workspace/tools/usr/bin/xz"); err == nil {
			t.Setenv("SQUISH_XZ", "/workspace/tools/usr/bin/xz")
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	base := bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog. "), 1200) // ~54 KB
	m1 := newManifest(t, st, "v1", map[string][]byte{"usr/lib/libdemo.so.1": base})
	m2 := newManifest(t, st, "v2", map[string][]byte{"usr/lib/libdemo.so.1": variant(base, 0x5A)})
	m3 := newManifest(t, st, "v3", map[string][]byte{"usr/lib/libdemo.so.1": variant(base, 0xA5)})

	conv, saved, err := DeltaPass(st, []*store.Manifest{m1, m2, m3}, 64, 1<<20)
	if err != nil {
		t.Fatalf("DeltaPass: %v", err)
	}
	if conv != 2 {
		t.Fatalf("want 2 converted entries, got %d (saved %d)", conv, saved)
	}
	if saved <= 0 {
		t.Fatalf("expected positive savings, got %d", saved)
	}
	// exactly two entries became deltas with a base; one stays a plain
	// file (the base)
	deltas, files := 0, 0
	for _, m := range []*store.Manifest{m1, m2, m3} {
		e := m.Files[0]
		switch e.Kind {
		case store.KindDelta:
			if e.Base == "" || e.Clen <= 0 {
				t.Fatalf("%s: bad delta entry: %+v", m.Name, e)
			}
			deltas++
		case store.KindFile:
			files++
		default:
			t.Fatalf("%s: unexpected kind %q", m.Name, e.Kind)
		}
	}
	if deltas != 2 || files != 1 {
		t.Fatalf("want 2 deltas + 1 file, got %d deltas + %d files", deltas, files)
	}
	// byte-exact materialize for all three
	for i, m := range []*store.Manifest{m1, m2, m3} {
		out := filepath.Join(t.TempDir(), "out")
		want := base
		if i == 1 {
			want = variant(base, 0x5A)
		} else if i == 2 {
			want = variant(base, 0xA5)
		}
		if _, err := Materialize(st, m, out, false, true); err != nil {
			t.Fatalf("materialize %s: %v", m.Name, err)
		}
		got, err := os.ReadFile(filepath.Join(out, "usr/lib/libdemo.so.1"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: materialized bytes differ", m.Name)
		}
		if checked, bad, err := Verify(st, m); err != nil || bad != 0 || checked != 1 {
			t.Fatalf("%s: verify checked=%d bad=%d err=%v", m.Name, checked, bad, err)
		}
	}
	// the two non-base blobs are gone after gc
	removed, reclaimed, err := GCAll(st)
	if err != nil {
		t.Fatalf("GCAll: %v", err)
	}
	if removed < 2 || reclaimed <= 0 {
		t.Fatalf("gc removed %d objects (%d bytes); want at least the 2 orphan blobs", removed, reclaimed)
	}
}

func TestDeltaSkipsWhenNotSmaller(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	// two unrelated contents at the same path: delta should lose and stay a blob
	a := bytes.Repeat([]byte{0x00}, 4096)
	b := bytes.Repeat([]byte{0xFF}, 4096)
	m1 := newManifest(t, st, "one", map[string][]byte{"bin/x": a})
	m2 := newManifest(t, st, "two", map[string][]byte{"bin/x": b})
	_, _, err = DeltaPass(st, []*store.Manifest{m1, m2}, 64, 1<<20)
	if err != nil {
		t.Fatalf("DeltaPass: %v", err)
	}
	if m2.Files[0].Kind != store.KindFile {
		t.Fatalf("expected no conversion when delta does not compress better, got kind %q", m2.Files[0].Kind)
	}
	if _, err := os.Stat(st.DeltaPath(m2.Files[0].Hash, m1.Files[0].Hash)); !os.IsNotExist(err) {
		t.Fatalf("loser delta frame should not remain on disk")
	}
}

func TestColdPass(t *testing.T) {
	xz := os.Getenv("SQUISH_XZ")
	if xz == "" {
		if _, err := os.Stat("/workspace/tools/usr/bin/xz"); err == nil {
			xz = "/workspace/tools/usr/bin/xz"
			t.Setenv("SQUISH_XZ", xz)
		} else if _, err := exec.LookPath("xz"); err != nil {
			t.Skip("no xz binary available")
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	// a fake aarch64 ELF: magic at 0, e_machine = 183 at offset 18
	elf := make([]byte, 3<<20)
	copy(elf, []byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
	elf[18], elf[19] = 183, 0 // EM_AARCH64
	for i := 0x20; i < len(elf); i++ {
		elf[i] = byte(i * 31)
	}
	m := newManifest(t, st, "big", map[string][]byte{"usr/bin/heavy": elf})
	conv, saved, err := ColdPass(st, m, 1000)
	if err != nil {
		t.Fatalf("ColdPass: %v", err)
	}
	if conv != 1 {
		t.Fatalf("want 1 converted, got %d (saved %d)", conv, saved)
	}
	e := m.Files[0]
	if e.Kind != store.KindCold || e.Filter != "bcj-arm64" || e.Clen <= 0 || e.Clen >= e.Size {
		t.Fatalf("cold entry looks wrong: %+v", e)
	}
	if checked, bad, err := Verify(st, m); err != nil || bad != 0 || checked != 1 {
		t.Fatalf("verify checked=%d bad=%d err=%v", checked, bad, err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if _, err := Materialize(st, m, out, false, true); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "usr/bin/heavy"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, elf) {
		t.Fatal("cold materialize is not byte-exact")
	}
	// x86 ELF must pick the x86 filter
	xelf := append([]byte(nil), elf...)
	xelf[18], xelf[19] = 62, 0
	if got := elfFilter(xelf); got != "x86" {
		t.Fatalf("x86 ELF filter = %q", got)
	}
	if got := elfFilter([]byte("not an elf")); got != "" {
		t.Fatalf("non-ELF filter = %q", got)
	}
	if e.Filter == "bcj-arm64" {
		t.Logf("native cold: %d -> %d bytes (%.1f%%)", e.Size, e.Clen, 100*float64(e.Clen)/float64(e.Size))
	}
}

func xzSupports(xzPath, filter string) bool {
	if xzPath == "" {
		return false
	}
	cmd := exec.Command(xzPath, "-c", "--"+filter)
	cmd.Stdin = bytes.NewReader([]byte("probe"))
	return cmd.Run() == nil
}

func TestGCProtectsDeltaBases(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	base := bytes.Repeat([]byte("base base base base "), 2000)
	m1 := newManifest(t, st, "a", map[string][]byte{"lib/x": base})
	m2 := newManifest(t, st, "b", map[string][]byte{"lib/x": variant(base, 1)})
	if _, _, err := DeltaPass(st, []*store.Manifest{m1, m2}, 64, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, _, err := GCAll(st); err != nil {
		t.Fatal(err)
	}
	// exactly one entry stays a plain file: that one is the base and its
	// blob must survive (the delta needs it to decode)
	var baseHash string
	for _, m := range []*store.Manifest{m1, m2} {
		e := m.Files[0]
		if e.Kind == store.KindFile {
			baseHash = e.Hash
		}
	}
	if baseHash == "" {
		t.Fatalf("no plain-file base left after delta pass")
	}
	if _, err := st.ReadBlob(baseHash); err != nil {
		t.Fatalf("delta base blob was collected: %v", err)
	}
	for _, m := range []*store.Manifest{m1, m2} {
		e := m.Files[0]
		if e.Kind != store.KindDelta {
			continue
		}
		if _, err := st.ReadDelta(e.Hash, e.Base); err != nil {
			t.Fatalf("delta undecodable after gc: %v", err)
		}
	}
}
