package ingest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloysiusvonfck/squish/internal/ops"
	"github.com/aloysiusvonfck/squish/internal/store"
)

// makeDockerSave builds a synthetic docker save archive with two layers:
// layer1 has base.txt, app/run.py and dir/one.txt; layer2 changes app/run.py,
// adds dir/two.txt and whiteouts dir/one.txt.
func makeDockerSave(t *testing.T, gz bool) string {
	t.Helper()
	type layer struct {
		name string
		tar  []byte
	}
	mk := func(name string, entries map[string]string, whiteouts []string) layer {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		names := make([]string, 0, len(entries))
		for k := range entries {
			names = append(names, k)
		}
		sortStrings(names)
		for _, fn := range names {
			body := entries[fn]
			hdr := &tar.Header{Name: fn, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
			tw.WriteHeader(hdr)
			tw.Write([]byte(body))
		}
		for _, w := range whiteouts {
			hdr := &tar.Header{Name: w, Mode: 0o644, Typeflag: tar.TypeReg}
			tw.WriteHeader(hdr)
		}
		tw.Close()
		return layer{name: name, tar: buf.Bytes()}
	}
	l1 := mk("abc123/layer.tar", map[string]string{
		"base.txt":   "base layer file\n",
		"app/run.py": "print('v1')\n",
		"dir/one.txt": "one\n",
	}, nil)
	l2 := mk("def456/layer.tar", map[string]string{
		"app/run.py": "print('v2')\n",
		"dir/two.txt": "two\n",
	}, []string{"dir/.wh.one.txt"})
	man, _ := json.Marshal([]dockerManifestEntry{{
		Config:   "cfg/config.json",
		RepoTags: []string{"synthetic:latest"},
		Layers:   []string{l1.name, l2.name},
	}})

	var archive bytes.Buffer
	var tw *tar.Writer
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(&archive)
		tw = tar.NewWriter(gzw)
	} else {
		tw = tar.NewWriter(&archive)
	}
	defer tw.Close()
	add := func(name string, body []byte) {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		tw.WriteHeader(hdr)
		tw.Write(body)
	}
	add(l1.name, l1.tar)
	add(l2.name, l2.tar)
	add("cfg/config.json", []byte("{}\n"))
	add("manifest.json", man)
	tw.Close()
	if gzw != nil {
		gzw.Close()
	}

	p := filepath.Join(t.TempDir(), "img.tar")
	if gz {
		p += ".gz"
	}
	if err := os.WriteFile(p, archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func testDockerSave(t *testing.T, gz bool) {
	src := makeDockerSave(t, gz)
	st, err := store.Open(t.TempDir() + "/store")
	if err != nil {
		t.Fatal(err)
	}
	m, err := IngestPath(st, src, "synthetic", IngestOptions{Elide: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	find := func(p string) *store.FileEntry {
		for i := range m.Files {
			if m.Files[i].Path == p {
				return &m.Files[i]
			}
		}
		return nil
	}
	if e := find("app/run.py"); e == nil || e.Hash == "" {
		t.Fatal("app/run.py missing or unhashed")
	} else if got, _ := st.ReadBlob(e.Hash); string(got) != "print('v2')\n" {
		t.Fatalf("run.py: layer 2 must win, got %q", got)
	}
	if e := find("dir/one.txt"); e != nil {
		t.Fatal("whiteout not applied to dir/one.txt")
	}
	if e := find("dir/two.txt"); e == nil {
		t.Fatal("dir/two.txt missing")
	}
	if e := find("base.txt"); e == nil || e.Hash == "" {
		t.Fatal("base.txt missing")
	}
	out := t.TempDir() + "/out"
	if _, err := ops.Materialize(st, m, out, true, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "app", "run.py"))
	if err != nil || string(b) != "print('v2')\n" {
		t.Fatalf("materialized run.py wrong: %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(out, "dir", "one.txt")); !os.IsNotExist(err) {
		t.Fatal("whiteout file materialized")
	}
	checked, bad, err := ops.Verify(st, m)
	if err != nil || bad != 0 || checked < 3 {
		t.Fatalf("verify checked=%d bad=%d err=%v", checked, bad, err)
	}
}

func TestDockerSave(t *testing.T)      { testDockerSave(t, false) }
func TestDockerSaveGz(t *testing.T)    { testDockerSave(t, true) }

func TestDedupAcrossCopies(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/store")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.MkdirAll(filepath.Join(dir, "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "same.txt"), []byte(strings.Repeat("dup", 500)), 0o644)
	os.WriteFile(filepath.Join(dir, "b", "same.txt"), []byte(strings.Repeat("dup", 500)), 0o644)
	m, err := IngestPath(st, dir, "dup", IngestOptions{Elide: false})
	if err != nil {
		t.Fatal(err)
	}
	if m.DedupBytes != 1500 {
		t.Fatalf("dedup savings = %d, want 1500", m.DedupBytes)
	}
}

func TestIngestRejectsUnsafePath(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil!")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir() + "/st"
	st, err := store.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	m := &store.Manifest{}
	if err := applyLayer(tr, map[string]store.FileEntry{}, m, st, IngestOptions{}); err == nil {
		t.Fatal("ingest accepted a layer path that escapes the layer root")
	}
}
