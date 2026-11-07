package ops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aloysiusvonfck/squish/internal/store"
)

func sweepFixture(t *testing.T) (*store.Store, map[string]map[string]string) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/st")
	if err != nil {
		t.Fatal(err)
	}
	content := map[string]map[string]string{"ma": {}, "mb": {}}
	put := func(mn, p, s string) store.FileEntry {
		content[mn][p] = s
		h, err := st.PutBlob([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return store.FileEntry{Path: p, Size: int64(len(s)), Mode: 0o644, Kind: store.KindFile, Hash: h}
	}
	shared := "shared payload that appears in both manifests with identical bytes and some repetition repetition repetition"
	ma := &store.Manifest{Name: "ma"}
	ma.Files = []store.FileEntry{
		put("ma", "etc/a.conf", "config A alpha alpha alpha alpha alpha"),
		put("ma", "usr/bin/tool", "tool body for image A tool body for image A tool body"),
		put("ma", "usr/share/doc/shared", shared),
		put("ma", "var/log/a.log", "log lines for a log lines for a log lines for a"),
	}
	mb := &store.Manifest{Name: "mb"}
	mb.Files = []store.FileEntry{
		put("mb", "etc/b.conf", "config B bravo bravo bravo bravo bravo"),
		put("mb", "usr/bin/tool", "tool body for image B tool body for image B tool body"),
		put("mb", "usr/share/doc/shared", shared),
		put("mb", "var/log/b.log", "log lines for b log lines for b log lines for b"),
	}
	for _, m := range []*store.Manifest{ma, mb} {
		if err := st.SaveManifest(m); err != nil {
			t.Fatal(err)
		}
	}
	return st, content
}

func checkSwept(t *testing.T, st *store.Store, ms []*store.Manifest, content map[string]map[string]string) {
	t.Helper()
	for _, m := range ms {
		checked, bad, err := Verify(st, m)
		if err != nil {
			t.Fatalf("verify %s: %v", m.Name, err)
		}
		if bad != 0 || checked != len(m.Files) {
			t.Fatalf("verify %s: checked %d bad %d want %d/0", m.Name, checked, bad, len(m.Files))
		}
		out := t.TempDir()
		if _, err := Materialize(st, m, out, false, false); err != nil {
			t.Fatalf("materialize %s: %v", m.Name, err)
		}
		for p, want := range content[m.Name] {
			got, err := os.ReadFile(filepath.Join(out, p))
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			if string(got) != want {
				t.Fatalf("file %s: content mismatch", p)
			}
		}
	}
}

func reloadTwo(t *testing.T, st *store.Store) []*store.Manifest {
	t.Helper()
	var out []*store.Manifest
	for _, n := range []string{"ma", "mb"} {
		m, err := st.LoadManifest(n)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestSweepManifestMode(t *testing.T) {
	st, content := sweepFixture(t)
	ms, files, frames, err := SweepStore(st, 1<<20, "manifest")
	if err != nil {
		t.Fatal(err)
	}
	if ms != 2 || files != 7 || frames != 1 {
		t.Fatalf("sweep: manifests %d files %d frames %d", ms, files, frames)
	}
	n, reclaimed, err := GCAll(st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 || reclaimed == 0 {
		t.Fatalf("gc: removed %d reclaimed %d", n, reclaimed)
	}
	checkSwept(t, st, reloadTwo(t, st), content)
}

func TestSweepPathMode(t *testing.T) {
	st, content := sweepFixture(t)
	if _, _, _, err := SweepStore(st, 1<<20, "path"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := GCAll(st); err != nil {
		t.Fatal(err)
	}
	checkSwept(t, st, reloadTwo(t, st), content)
}

func TestSweepMultiRoll(t *testing.T) {
	st, content := sweepFixture(t)
	ms, _, frames, err := SweepStore(st, 64, "manifest")
	if err != nil {
		t.Fatal(err)
	}
	if frames < 2 {
		t.Fatalf("expected several rolls, got %d (manifests %d)", frames, ms)
	}
	if _, _, err := GCAll(st); err != nil {
		t.Fatal(err)
	}
	checkSwept(t, st, reloadTwo(t, st), content)
}

func TestSweepLeavesNonFileEntries(t *testing.T) {
	st, content := sweepFixture(t)
	ma, err := st.LoadManifest("ma")
	if err != nil {
		t.Fatal(err)
	}
	ma.Files = append(ma.Files,
		store.FileEntry{Path: "bin/link", Mode: 0o777, Kind: store.KindSymlink, Link: "etc/a.conf"},
		store.FileEntry{Path: "var/cache/junk", Size: 12, Kind: store.KindElided, Reason: "regen"},
	)
	ma.RawBytes += 12
	if err := st.SaveManifest(ma); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := SweepStore(st, 1<<20, "manifest"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := GCAll(st); err != nil {
		t.Fatal(err)
	}
	ma, err = st.LoadManifest("ma")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := manifestHas(ma, "bin/link")
	if !ok || e.Frame != "" || e.Link != "etc/a.conf" {
		t.Fatalf("symlink entry mutated: %+v", e)
	}
	e, ok = manifestHas(ma, "var/cache/junk")
	if !ok || e.Kind != store.KindElided {
		t.Fatalf("elided entry mutated: %+v", e)
	}
	_, bad, err := Verify(st, ma)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("verify bad %d", bad)
	}
	out := t.TempDir()
	if _, err := Materialize(st, ma, out, false, false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(out, "bin/link")); err != nil || got != "etc/a.conf" {
		t.Fatalf("symlink materialized wrong: %q %v", got, err)
	}
	_ = content
}

func manifestHas(m *store.Manifest, p string) (store.FileEntry, bool) {
	for _, e := range m.Files {
		if e.Path == p {
			return e, true
		}
	}
	return store.FileEntry{}, false
}
