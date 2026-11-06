package sqfs

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/aloysiusvonfck/squish/internal/store"
)

func TestTreeStructure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	m := &store.Manifest{Name: "t", Source: "test"}
	add := func(p string, data []byte) {
		h, err := st.PutBlob(data)
		if err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, store.FileEntry{Path: p, Size: int64(len(data)), Mode: 0o755, Kind: store.KindFile, Hash: h})
	}
	add("usr/bin/run", []byte("bin-payload"))
	add("etc/app.conf", []byte("conf=1"))
	m.Files = append(m.Files, store.FileEntry{Path: "usr/lib", Kind: store.KindDir})
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}

	tr := NewTree(st, m)
	if !tr.IsDir("usr") || !tr.IsDir("usr/lib") {
		t.Fatal("implicit and explicit dirs missing")
	}
	if tr.IsDir("usr/bin/run") {
		t.Fatal("file seen as dir")
	}
	if e, ok := tr.File("usr/bin/run"); !ok || e.Hash == "" {
		t.Fatal("file lookup failed")
	}
	kids := tr.Children(".")
	want := []string{"etc", "usr"}
	if len(kids) != len(want) || kids[0] != want[0] || kids[1] != want[1] {
		t.Fatalf("root children = %v, want %v", kids, want)
	}
	kids = tr.Children("usr")
	if len(kids) != 2 || kids[0] != "bin" || kids[1] != "lib" {
		t.Fatalf("usr children = %v", kids)
	}
	data, err := tr.ReadFile(tr.mustEntry(t, "usr/bin/run"))
	if err != nil || !bytes.Equal(data, []byte("bin-payload")) {
		t.Fatalf("read via tree: %v %q", err, data)
	}
}

// mustEntry fetches the entry pointer for a path (test helper).
func (t *Tree) mustEntry(tb *testing.T, p string) *store.FileEntry {
	e, ok := t.File(p)
	if !ok {
		tb.Fatalf("no entry for %s", p)
	}
	return &e
}

func TestTreeElidedReadFails(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	m := &store.Manifest{Name: "e", Source: "test"}
	m.Files = append(m.Files, store.FileEntry{Path: "proc/meminfo", Size: 10, Kind: store.KindElided, Reason: "proc"})
	tr := NewTree(st, m)
	e := tr.mustEntry(t, "proc/meminfo")
	if _, err := tr.ReadFile(e); err == nil {
		t.Fatal("elided file read should fail")
	}
}
