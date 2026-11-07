package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aloysiusvonfck/squish/internal/ops"
	"github.com/aloysiusvonfck/squish/internal/store"
)

// writeTwoImageDocker builds a fake docker whose two images share one file
// (etc/shared.txt) and differ everywhere else, with distinct image configs.
func writeTwoImageDocker(t *testing.T) (bin string, log string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "docker")
	log = filepath.Join(dir, "log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
case "$1" in
images)
  printf 'demo1:latest|34.2MB\ndemo2:latest|34.2MB\n' ;;
save)
  # $2=-o $3=FILE $4=REF
  d=$(mktemp -d)
  printf '{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/bin","IMG=%%s"]},"rootfs":{"diff_ids":["sha256:old"]}}' "$4" > "$d/config.json"
  mkdir -p "$d/layerdir/etc"
  # big mostly-identical bodies so the delta pass has something to win
  head -c 500000 /dev/zero | tr '\0' 'X' > "$d/layerdir/etc/motd"
  echo "hello from $4" >> "$d/layerdir/etc/motd"
  echo "shared content" > "$d/layerdir/etc/shared.txt"
  head -c 500000 /dev/zero | tr '\0' 'Y' > "$d/layerdir/etc/variant.txt"
  echo "variant $4" >> "$d/layerdir/etc/variant.txt"
  tar -cf "$d/layer1.tar" -C "$d/layerdir" etc
  mkdir -p "$d/layer1"
  mv "$d/layer1.tar" "$d/layer1/layer.tar"
  cfgsha=$(sha256sum "$d/config.json" | cut -d' ' -f1)
  mv "$d/config.json" "$d/$cfgsha.json"
  printf '[{"Config":"%%s.json","RepoTags":["%%s"],"Layers":["layer1/layer.tar"]}]' "$cfgsha" "$4" > "$d/manifest.json"
  tar -cf "$3" -C "$d" manifest.json "$cfgsha.json" layer1
  rm -rf "$d" ;;
rmi)
  echo "rmi $2" >> %s ;;
load)
  f="$3"
  cfg=$(tar -xOf "$f" config.json)
  echo "$cfg" | grep -q '"diff_ids"' || { echo "no diff_ids"; exit 1; }
  lay=$(tar -xOf "$f" squish/layer.tar | sha256sum | cut -d' ' -f1)
  echo "$cfg" | grep -q "sha256:$lay" || { echo "diff_id mismatch: sha256:$lay"; exit 1; }
  tar -xOf "$f" manifest.json | grep -q 'squish/layer.tar' || { echo "manifest.json wrong"; exit 1; }
  echo "LOAD-OK" >> %s ;;
*)
  echo "fake docker: unknown $1"; exit 1 ;;
esac`, log, log, log)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func loadAccountingStore(t *testing.T) (*store.Store, []*store.Manifest) {
	t.Helper()
	bin, _ := writeTwoImageDocker(t)
	stRoot := t.TempDir() + "/store"
	t.Setenv("SQUISH_DOCKER", bin)
	t.Setenv("SQUISH_STORE", stRoot)
	if err := cmdReclaim(nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	st, err := store.Open(stRoot)
	if err != nil {
		t.Fatal(err)
	}
	names, err := st.ListManifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("want 2 manifests, got %d: %v", len(names), names)
	}
	var ms []*store.Manifest
	for _, n := range names {
		m, err := st.LoadManifest(n)
		if err != nil {
			t.Fatal(err)
		}
		ms = append(ms, m)
	}
	return st, ms
}

func TestAccountingSharedAndUnique(t *testing.T) {
	st, ms := loadAccountingStore(t)
	acc, err := st.AccountStore(ms)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if len(acc.Images) != 2 {
		t.Fatalf("want 2 image rows, got %d", len(acc.Images))
	}
	var sumStored int64
	for _, ia := range acc.Images {
		if ia.Stored != ia.Unique+ia.Shared {
			t.Errorf("%s: stored %d != unique %d + shared %d", ia.Name, ia.Stored, ia.Unique, ia.Shared)
		}
		if ia.Unique == 0 {
			t.Errorf("%s: expected some unique bytes", ia.Name)
		}
		if ia.Shared == 0 {
			t.Errorf("%s: expected some shared bytes (etc/shared.txt)", ia.Name)
		}
		sumStored += ia.Stored
	}
	if acc.Physical >= sumStored {
		t.Errorf("physical %d should be below the naive sum %d (shared counted once)", acc.Physical, sumStored)
	}
	if acc.OrphanCount != 0 || acc.OrphanBytes != 0 {
		t.Errorf("expected no orphans, got %d files / %d bytes", acc.OrphanCount, acc.OrphanBytes)
	}
}

func TestAccountingMissingObjectErrors(t *testing.T) {
	st, ms := loadAccountingStore(t)
	// find a blob referenced only by ms[0] and remove it from disk; the
	// other manifest must still account cleanly
	other := map[string]bool{}
	for _, e := range ms[1].Files {
		for _, key := range []string{e.Hash, e.Base, e.Frame} {
			if key != "" {
				other[key] = true
			}
		}
	}
	if ms[1].ImageConfig != "" {
		other[ms[1].ImageConfig] = true
	}
	m := ms[0]
	var victim string
	for _, e := range m.Files {
		if e.Kind == store.KindFile && e.Hash != m.ImageConfig && !other[e.Hash] {
			victim = e.Hash
			break
		}
	}
	if victim == "" {
		t.Fatal("no uniquely-owned blob found")
	}
	if err := os.Remove(filepath.Join(st.Root, "blobs", victim[:2], victim+".zst")); err != nil {
		t.Fatal(err)
	}
	acc, err := st.AccountStore(ms)
	if err != nil {
		t.Fatalf("one broken manifest must not sink the store: %v", err)
	}
	var broken, good int
	for _, ia := range acc.Images {
		if ia.OK {
			good++
		} else {
			broken++
			if !contains(ia.Err, m.Name) {
				t.Errorf("error should name the manifest %q, got: %s", m.Name, ia.Err)
			}
		}
	}
	if broken != 1 || good != len(ms)-1 {
		t.Errorf("want 1 broken / %d good, got %d broken / %d good", len(ms)-1, broken, good)
	}
	if acc.UnresolvedKeys == 0 {
		t.Error("expected unresolved keys counted")
	}
	// an object a broken manifest names is never an orphan
	if acc.OrphanCount != 0 {
		t.Errorf("broken manifest's other objects must not become orphans, got %d", acc.OrphanCount)
	}
	// all-broken store still errors
	if _, err := st.AccountStore(ms[:1]); err == nil {
		t.Error("expected an error when no manifest accounts cleanly")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestAccountingOrphansAfterManifestRemoval(t *testing.T) {
	st, ms := loadAccountingStore(t)
	before, err := st.AccountStore(ms)
	if err != nil {
		t.Fatal(err)
	}
	// remove one manifest: its unique blobs become gc candidates
	victim := ms[1].Name
	if err := os.Remove(st.ManifestPath(victim)); err != nil {
		t.Fatal(err)
	}
	names, _ := st.ListManifests()
	var left []*store.Manifest
	for _, n := range names {
		m, err := st.LoadManifest(n)
		if err != nil {
			t.Fatal(err)
		}
		left = append(left, m)
	}
	after, err := st.AccountStore(left)
	if err != nil {
		t.Fatal(err)
	}
	if after.OrphanCount == 0 || after.OrphanBytes == 0 {
		t.Fatalf("expected orphans after removing %s, got %d files / %d bytes", victim, after.OrphanCount, after.OrphanBytes)
	}
	if after.OrphanBytes >= before.Physical {
		t.Errorf("orphan bytes %d should be well under total physical %d", after.OrphanBytes, before.Physical)
	}
}

func TestAccountingAfterDeltaAndGC(t *testing.T) {
	st, ms := loadAccountingStore(t)
	conv, _, err := ops.DeltaPass(st, ms, 1, 1<<30)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if conv == 0 {
		t.Fatal("expected some files converted to deltas")
	}
	if _, _, err := ops.GCAll(st); err != nil {
		t.Fatalf("gc: %v", err)
	}
	acc, err := st.AccountStore(ms)
	if err != nil {
		t.Fatalf("account after delta+gc: %v", err)
	}
	for _, ia := range acc.Images {
		if ia.Stored == 0 || ia.Stored != ia.Unique+ia.Shared {
			t.Errorf("%s: stored %d, unique %d, shared %d", ia.Name, ia.Stored, ia.Unique, ia.Shared)
		}
	}
	// the accounting must describe a store that still restores byte-exact
	for _, m := range ms {
		out := t.TempDir() + "/out"
		if err := cmdMaterialize([]string{"--store", st.Root, m.Name, out}); err != nil {
			t.Fatalf("materialize %s: %v", m.Name, err)
		}
		want := "hello from " + m.RepoTags[0] + "\n"
		got, err := os.ReadFile(filepath.Join(out, "etc", "motd"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 500000+len(want) || string(got[500000:]) != want {
			t.Errorf("%s motd: len %d, tail %q", m.Name, len(got), string(got[max(0, len(got)-40):]))
		}
		got, err = os.ReadFile(filepath.Join(out, "etc", "shared.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "shared content\n" {
			t.Errorf("%s shared.txt: got %q", m.Name, got)
		}
	}
}
