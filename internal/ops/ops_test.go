package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloysiusvonfck/squish/internal/ingest"
	"github.com/aloysiusvonfck/squish/internal/store"
)

func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	// repetitive text so dictionary grouping has something to bite on
	tpl := "server { listen 80; server_name %s; root /var/www; access_log /var/log/nginx/%s.log; keepalive_timeout 65; gzip on; location / { try_files $uri $uri/ /index.html; } }"
	sites := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima"}
	for _, s := range sites {
		mk("etc/nginx/sites/"+s+".conf", strings.ReplaceAll(tpl, "%s", s))
	}
	for i := 0; i < 10; i++ {
		mk("var/cache/apt/archives/pkg"+string(rune('a'+i))+".list", "deb http://deb.debian.org/debian bookworm main # package list "+strings.Repeat("x", 200))
	}
	mk("app/main.py", "import sys\n\n\ndef main():\n    print('hello squish')\n\n\nif __name__ == '__main__':\n    main()\n")
	mk("app/__pycache__/main.cpython-312.pyc", "fake-bytecode-never-stored")
	mk("usr/share/doc/nginx/README", "docs about nginx")
	os.Symlink("/etc/nginx/nginx.conf", filepath.Join(root, "etc", "nginx", "link.conf"))
	return root
}

func roundtrip(t *testing.T, elide bool) (*store.Store, *store.Manifest, string, string) {
	t.Helper()
	src := buildTree(t)
	ws := t.TempDir() + "/store"
	st, err := store.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ingest.IngestPath(st, src, "test", ingest.IngestOptions{Elide: elide})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir() + "/out"
	n, err := Materialize(st, m, out, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("nothing materialized")
	}
	return st, m, src, out
}

func compareTrees(t *testing.T, src, out string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		dst := filepath.Join(out, rel)
		df, derr := os.Lstat(dst)
		if derr != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if df.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("%s: expected symlink", rel)
			}
			l1, _ := os.Readlink(p)
			l2, _ := os.Readlink(dst)
			if l1 != l2 {
				t.Fatalf("%s: symlink target changed", rel)
			}
			return nil
		}
		if fi.IsDir() {
			return nil
		}
		if fi.Mode().IsRegular() {
			b1, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b2, err := os.ReadFile(dst)
			if err != nil {
				return err
			}
			if string(b1) != string(b2) {
				t.Fatalf("%s: content mismatch", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestElision(t *testing.T) {
	_, m, _, out := roundtrip(t, true)
	var elided int
	for _, e := range m.Files {
		if e.Kind == store.KindElided {
			elided++
			if _, err := os.Lstat(filepath.Join(out, filepath.FromSlash(e.Path))); err == nil {
				t.Fatalf("elided file %s materialized anyway", e.Path)
			}
		}
	}
	if elided < 2 {
		t.Fatalf("expected pyc + docs elided, got %d", elided)
	}
	if m.ElidedBytes == 0 {
		t.Fatal("elided bytes not tallied")
	}
}

func TestVerify(t *testing.T) {
	st, m, _, _ := roundtrip(t, false)
	checked, bad, err := Verify(st, m)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 0 || checked == 0 {
		t.Fatalf("verify: checked=%d bad=%d", checked, bad)
	}
}

func TestRepackRoundTrip(t *testing.T) {
	st, m, src, _ := roundtrip(t, false)
	before := m.StoredBytes
	if _, moved, err := Repack(st, m, 256*1024, 8, false); err != nil {
		t.Fatal(err)
	} else if moved == 0 {
		t.Fatal("repack moved nothing")
	}
	// materialize again after repack: every entry now lives in frames
	out2 := t.TempDir() + "/out2"
	if _, err := Materialize(st, m, out2, true, true); err != nil {
		t.Fatal(err)
	}
	compareTrees(t, src, out2)
	checked, bad, err := Verify(st, m)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 0 || checked == 0 {
		t.Fatalf("post-repack verify: checked=%d bad=%d", checked, bad)
	}
	if m.StoredBytes > before {
		t.Logf("note: repack grew store %d -> %d (random-ish data)", before, m.StoredBytes)
	}
}

func TestStats(t *testing.T) {
	st, m, _, _ := roundtrip(t, false)
	if err := Stats(st, m); err != nil {
		t.Fatal(err)
	}
}

func TestHardlinkMaterialize(t *testing.T) {
	st, err0 := store.Open(t.TempDir() + "/st")
	if err0 != nil {
		t.Fatal(err0)
	}
	content := []byte("the real content of the shared file")
	hash, err := st.PutBlob(content)
	if err != nil {
		t.Fatal(err)
	}
	// the link entry sorts before its target, exercising the deferred pass
	m := &store.Manifest{Name: "hl", Files: []store.FileEntry{
		{Path: "app", Mode: 0o755, Kind: store.KindDir},
		{Path: "app/hardlink.txt", Mode: 0o644, Kind: store.KindLink, Link: "app/original.txt"},
		{Path: "app/original.txt", Size: int64(len(content)), Mode: 0o644, Kind: store.KindFile, Hash: hash},
	}}
	out := t.TempDir() + "/out"
	files, err := Materialize(st, m, out, true, true)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if files != 2 { // original + hardlink
		t.Fatalf("files = %d, want 2", files)
	}
	a, err := os.ReadFile(out + "/app/original.txt")
	if err != nil || string(a) != string(content) {
		t.Fatalf("target wrong: %v %q", err, a)
	}
	b, err := os.ReadFile(out + "/app/hardlink.txt")
	if err != nil || string(b) != string(content) {
		t.Fatalf("hardlink wrong: %v %q", err, b)
	}
	ia, _ := os.Stat(out + "/app/original.txt")
	ib, _ := os.Stat(out + "/app/hardlink.txt")
	if !os.SameFile(ia, ib) {
		t.Fatal("hardlink did not share an inode")
	}
}

func TestGCKeepsImageConfig(t *testing.T) {
	st, m, _, _ := roundtrip(t, false)
	hash, err := st.PutBlob([]byte(`{"architecture":"amd64","os":"linux"}`))
	if err != nil {
		t.Fatal(err)
	}
	m.ImageConfig = hash
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	if _, _, err := GCAll(st); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if _, err := st.ReadBlob(hash); err != nil {
		t.Fatalf("gc deleted the image config blob: %v", err)
	}
}

func TestMaterializeRejectsEscape(t *testing.T) {
	st, _, _, _ := roundtrip(t, false)
	hash, err := st.PutBlob([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	m := &store.Manifest{Name: "evil"}
	m.Files = append(m.Files, store.FileEntry{Path: "../evil.txt", Size: 7, Mode: 0o644, Kind: store.KindFile, Hash: hash})
	out := t.TempDir()
	if _, err := Materialize(st, m, out, false, false); err == nil {
		t.Fatal("materialize accepted an escaping path")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(out), "evil.txt")); err == nil {
		t.Error("materialize wrote outside the output directory")
	}
}

func TestVerifyChecksImageConfig(t *testing.T) {
	st, m, _, _ := roundtrip(t, false)
	hash, err := st.PutBlob([]byte(`{"architecture":"amd64","os":"linux","rootfs":{"diff_ids":["sha256:x"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	m.ImageConfig = hash
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	if _, bad, err := Verify(st, m); err != nil || bad != 0 {
		t.Fatalf("clean config: bad=%d err=%v", bad, err)
	}
	// Tamper with the stored blob: a damaged config must fail verification.
	blob := filepath.Join(st.Root, "blobs", hash[:2], hash+".zst")
	if err := os.WriteFile(blob, []byte("garbage-not-zstd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, bad, err := Verify(st, m); err == nil && bad == 0 {
		t.Fatal("verify accepted a tampered image config blob")
	}
}

func TestMaterializeRefusesSymlinkParent(t *testing.T) {
	st, _, _, _ := roundtrip(t, false)
	hash, err := st.PutBlob([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	outside := t.TempDir()
	m := &store.Manifest{Name: "esc"}
	m.Files = []store.FileEntry{
		{Path: "var", Kind: store.KindDir, Mode: 0o755},
		{Path: "var/run", Kind: store.KindSymlink, Link: outside},
		{Path: "var/run/created/nested/payload", Size: 7, Mode: 0o644, Kind: store.KindFile, Hash: hash},
	}
	if _, err := Materialize(st, m, out, false, false); err == nil {
		t.Fatal("materialize wrote through a symlinked parent")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("files appeared outside the output directory: %v", entries)
	}
}

func TestMaterializeRefusesSymlinkParentForSymlinkEntry(t *testing.T) {
	st, _, _, _ := roundtrip(t, false)
	out := t.TempDir()
	outside := t.TempDir()
	m := &store.Manifest{Name: "symlink-parent"}
	m.Files = []store.FileEntry{
		{Path: "var", Kind: store.KindDir, Mode: 0o755},
		{Path: "var/run", Kind: store.KindSymlink, Link: outside},
		{Path: "var/run/created/link", Kind: store.KindSymlink, Link: "/tmp/target"},
	}
	if _, err := Materialize(st, m, out, false, false); err == nil {
		t.Fatal("materialize created a symlink through a symlinked parent")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("entries appeared outside the output directory: %v", entries)
	}
}

func TestMaterializeKeepsAbsoluteSymlink(t *testing.T) {
	st, _, _, _ := roundtrip(t, false)
	out := t.TempDir()
	m := &store.Manifest{Name: "sym"}
	m.Files = []store.FileEntry{
		{Path: "usr", Kind: store.KindDir, Mode: 0o755},
		{Path: "usr/share", Kind: store.KindDir, Mode: 0o755},
		{Path: "usr/share/zoneinfo", Kind: store.KindSymlink, Link: "/usr/lib/zoneinfo"},
	}
	if _, err := Materialize(st, m, out, false, false); err != nil {
		t.Fatalf("materialize refused a legitimate absolute symlink: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(out, "usr/share/zoneinfo")); err != nil || got != "/usr/lib/zoneinfo" {
		t.Errorf("symlink = %q, err %v", got, err)
	}
}

func TestHardlinkChain(t *testing.T) {
	st, _, _, _ := roundtrip(t, false)
	hash, err := st.PutBlob([]byte("X"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	m := &store.Manifest{Name: "chain"}
	m.Files = []store.FileEntry{
		{Path: "a.txt", Size: 1, Mode: 0o644, Kind: store.KindFile, Hash: hash},
		{Path: "b.txt", Mode: 0o644, Kind: store.KindLink, Link: "a.txt"},
		{Path: "c.txt", Mode: 0o644, Kind: store.KindLink, Link: "b.txt"},
	}
	files, err := Materialize(st, m, out, true, false)
	if err != nil {
		t.Fatalf("chain materialize: %v", err)
	}
	if files != 3 {
		t.Errorf("files = %d, want 3", files)
	}
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(out, n)); err != nil {
			t.Errorf("%s missing: %v", n, err)
		}
	}
	a, _ := os.Stat(filepath.Join(out, "a.txt"))
	c, _ := os.Stat(filepath.Join(out, "c.txt"))
	if !os.SameFile(a, c) {
		t.Error("chain end is not the same inode as the base file")
	}
}

func TestHardlinkCycle(t *testing.T) {
	st, _, _, _ := roundtrip(t, false)
	out := t.TempDir()
	m := &store.Manifest{Name: "cyc"}
	m.Files = []store.FileEntry{
		{Path: "x", Mode: 0o644, Kind: store.KindLink, Link: "y"},
		{Path: "y", Mode: 0o644, Kind: store.KindLink, Link: "x"},
	}
	if _, err := Materialize(st, m, out, true, false); err == nil {
		t.Fatal("materialize accepted a hardlink cycle")
	}
}
