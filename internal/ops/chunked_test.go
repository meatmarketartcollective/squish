package ops

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/aloysiusvonfck/squish/internal/ingest"
	"github.com/aloysiusvonfck/squish/internal/store"
)

// chunkTree builds a tree with files large enough to chunk, two of which
// share a large interior region.
func chunkTree(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	mk := func(rel string, data []byte) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pattern := func(tag byte, n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = tag + byte(i%251)
		}
		return b
	}
	// shared interior region so chunk dedup has something to bite on
	shared := pattern('S', 48*1024)
	a := append(append(pattern('A', 16*1024), shared...), pattern('B', 24*1024)...)
	b := append(append(pattern('A', 16*1024), shared...), pattern('C', 24*1024)...)
	mk("usr/lib/big-a.bin", a)
	mk("usr/lib/big-b.bin", b)
	mk("etc/small.conf", []byte("tiny config")) // below the chunk threshold
	return root, "usr/lib/big-a.bin", "usr/lib/big-b.bin"
}

func fileBytes(t *testing.T, root, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestChunkedIngestRoundTrip(t *testing.T) {
	src, f1, f2 := chunkTree(t)
	st, err := store.Open(t.TempDir() + "/store")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ingest.IngestPath(st, src, "t", ingest.IngestOptions{Chunk: true})
	if err != nil {
		t.Fatal(err)
	}
	chunked := 0
	for _, e := range m.Files {
		if e.Kind == store.KindChunked {
			chunked++
			if e.Hash == "" || len(e.Chunks) == 0 {
				t.Fatalf("%s: chunked entry missing hash or chunks", e.Path)
			}
		}
	}
	if chunked != 2 {
		t.Fatalf("want 2 chunked entries, got %d", chunked)
	}
	if m.ChunkDedupBytes == 0 {
		t.Fatalf("expected cross-file chunk dedup, got 0")
	}
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir() + "/out"
	if _, err := Materialize(st, m, out, true, true); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{f1, f2, "etc/small.conf"} {
		want := fileBytes(t, src, f)
		got := fileBytes(t, out, f)
		if !bytes.Equal(want, got) {
			t.Fatalf("%s: materialize mismatch", f)
		}
	}
	if checked, bad, err := Verify(st, m); err != nil || bad > 0 {
		t.Fatalf("verify: checked=%d bad=%d err=%v", checked, bad, err)
	}
}

func TestRepackChunkFrames(t *testing.T) {
	src, f1, f2 := chunkTree(t)
	st, err := store.Open(t.TempDir() + "/store")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ingest.IngestPath(st, src, "t", ingest.IngestOptions{Chunk: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	groups, moved, err := Repack(st, m, 1<<20, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if moved < 2 {
		t.Fatalf("repack moved %d entries, want >= 2 (chunked entries)", moved)
	}
	framed := 0
	for _, e := range m.Files {
		if e.Kind != store.KindChunked {
			continue
		}
		framed++
		for _, c := range e.Chunks {
			if c.Frame == "" {
				t.Fatalf("%s: chunk %s not framed after repack", e.Path, c.Hash[:12])
			}
		}
	}
	if framed != 2 {
		t.Fatalf("want 2 chunked entries, got %d", framed)
	}
	if groups < 1 {
		t.Fatalf("want at least one chunk frame, got %d", groups)
	}
	// repack itself reclaims the now-unreferenced chunk blobs
	c0 := m.Files[0]
	if c0.Kind != store.KindChunked {
		for _, e := range m.Files {
			if e.Kind == store.KindChunked {
				c0 = e
				break
			}
		}
	}
	blobPath := filepath.Join(st.Root, "blobs", c0.Chunks[0].Hash[:2], c0.Chunks[0].Hash+".zst")
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Fatalf("chunk blob still on disk after repack reclaim")
	}
	out := t.TempDir() + "/out2"
	if _, err := Materialize(st, m, out, true, true); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{f1, f2} {
		want := fileBytes(t, src, f)
		got := fileBytes(t, out, f)
		if !bytes.Equal(want, got) {
			t.Fatalf("%s: post-repack materialize mismatch", f)
		}
	}
	if checked, bad, err := Verify(st, m); err != nil || bad > 0 {
		t.Fatalf("post-repack verify: checked=%d bad=%d err=%v", checked, bad, err)
	}
}

func TestChunkedAccounting(t *testing.T) {
	src, _, _ := chunkTree(t)
	st, err := store.Open(t.TempDir() + "/store")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ingest.IngestPath(st, src, "t", ingest.IngestOptions{Chunk: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	// StoredBytes must be positive and bounded by raw content
	if m.StoredBytes <= 0 || m.StoredBytes > m.RawBytes {
		t.Fatalf("StoredBytes %d out of range (raw %d)", m.StoredBytes, m.RawBytes)
	}
	// stats must run cleanly over chunked entries
	if err := Stats(st, m); err != nil {
		t.Fatal(err)
	}
}

func TestManifestCompressedV2(t *testing.T) {
	src, _, _ := chunkTree(t)
	st, err := store.Open(t.TempDir() + "/store")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ingest.IngestPath(st, src, "t", ingest.IngestOptions{Chunk: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(st.ManifestPath("t")); err != nil || fi.Size() == 0 {
		t.Fatalf("compressed manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.Root, "manifests", "t.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy manifest still present: %v", err)
	}
	m2, err := st.LoadManifest("t")
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Files) != len(m.Files) || m2.RawBytes != m.RawBytes {
		t.Fatal("manifest round trip mismatch")
	}
	// a legacy plain-JSON manifest still loads
	b, err := os.ReadFile(st.ManifestPath("t"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := store.DecodeAll(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Root, "manifests", "t.json"), plain, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(st.ManifestPath("t")); err != nil {
		t.Fatal(err)
	}
	m3, err := st.LoadManifest("t")
	if err != nil {
		t.Fatal(err)
	}
	if len(m3.Files) != len(m.Files) {
		t.Fatal("legacy manifest load mismatch")
	}
	names, err := st.ListManifests()
	if err != nil || len(names) != 1 || names[0] != "t" {
		t.Fatalf("ListManifests = %v, %v", names, err)
	}
}

func TestGCKeepsChunkBlobs(t *testing.T) {
	src, _, _ := chunkTree(t)
	st, err := store.Open(t.TempDir() + "/store")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ingest.IngestPath(st, src, "t", ingest.IngestOptions{Chunk: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	// a bogus blob nobody references must go; chunk blobs must stay
	bogus := filepath.Join(st.Root, "blobs", "zz", "0000000000000000000000000000000000000000000000000000000000000000.zst")
	os.MkdirAll(filepath.Dir(bogus), 0o755)
	if err := os.WriteFile(bogus, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, _, err := GCAll(st)
	if err != nil {
		t.Fatal(err)
	}
	if removed == 0 {
		t.Fatalf("gc removed nothing")
	}
	if _, err := os.Stat(bogus); !os.IsNotExist(err) {
		t.Fatal("gc left an unreferenced blob")
	}
	// every chunk blob of the manifest still readable
	if _, bad, err := Verify(st, m); err != nil || bad > 0 {
		t.Fatalf("verify after gc: bad=%d err=%v", bad, err)
	}
}
