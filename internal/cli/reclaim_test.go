package cli

// The reclaim/restore round trip runs against a fake docker: a shell
// script that pretends to be the docker CLI. Its `save` builds a real
// docker-save archive (manifest.json, config.json, one layer tar), its
// `load` validates the archive squish rebuilds: config.json must parse
// and its rootfs.diff_ids must equal the sha256 of the layer tar it
// carries, and manifest.json must list that layer. That is the same
// structural check a real docker load does, short of a live daemon.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeDocker(t *testing.T) (bin string, log string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "docker")
	log = filepath.Join(dir, "log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
case "$1" in
images)
  printf 'alpine:latest|34.2MB\nbusybox:1.36|1.2GB\n<none>:<none>|10MB\n' ;;
save)
  # $2=-o $3=FILE $4=REF
  d=$(mktemp -d)
  # real docker layout: the config is named <hash>.json and manifest.json
  # points to it through its Config field
  printf '{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/bin"]}}' > "$d/config.json"
  mkdir -p "$d/layerdir/etc"
  echo hello > "$d/layerdir/etc/motd"
  printf 'shared content' > "$d/layerdir/etc/shared.txt"
  ln "$d/layerdir/etc/shared.txt" "$d/layerdir/etc/shared-link.txt"
  tar -cf "$d/layer1.tar" -C "$d/layerdir" etc
  mkdir -p "$d/layer1"
  mv "$d/layer1.tar" "$d/layer1/layer.tar"
  cfgsha=$(sha256sum "$d/config.json" | cut -d' ' -f1)
  mv "$d/config.json" "$d/$cfgsha.json"
  printf '[{"Config":"'"$cfgsha.json"'","RepoTags":["%%s"],"Layers":["layer1/layer.tar"]}]' "$4" > "$d/manifest.json"
  tar -cf "$3" -C "$d" manifest.json "$cfgsha.json" layer1
  rm -rf "$d" ;;
rmi)
  echo "rmi $2" >> %s ;;
load)
  f="$3"   # $1=load $2=-i $3=FILE
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

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestReclaimRestoreRoundTrip(t *testing.T) {
	bin, log := writeFakeDocker(t)
	stRoot := t.TempDir() + "/store"
	t.Setenv("SQUISH_DOCKER", bin)
	t.Setenv("SQUISH_STORE", stRoot)

	if err := cmdReclaim(nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	l := readLog(t, log)
	for _, want := range []string{"save -o", "alpine:latest", "busybox:1.36", "rmi alpine:latest", "rmi busybox:1.36"} {
		if !strings.Contains(l, want) {
			t.Errorf("reclaim log missing %q in:\n%s", want, l)
		}
	}
	if strings.Contains(l, "<none>") {
		t.Errorf("reclaim touched an untagged image")
	}
	st, err := openStore("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.LoadManifest("alpine_latest")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if m.ImageConfig == "" {
		t.Error("reclaim did not save the image config")
	}
	if len(m.RepoTags) != 1 || m.RepoTags[0] != "alpine:latest" {
		t.Errorf("repo tags = %v", m.RepoTags)
	}

	if err := cmdLs(nil); err != nil {
		t.Fatalf("ls: %v", err)
	}

	if err := cmdRestore([]string{"alpine:latest"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	l = readLog(t, log)
	if !strings.Contains(l, "load -i") {
		t.Errorf("restore never called docker load; log:\n%s", l)
	}
	if !strings.Contains(l, "LOAD-OK") {
		t.Errorf("the rebuilt archive failed the load check; log:\n%s", l)
	}
	if _, err := os.Stat(stRoot + "/store/tmp/alpine:latest-image.tar"); err == nil {
	} else if _, err2 := os.Stat(stRoot + "/tmp/alpine:latest-image.tar"); err2 == nil {
		t.Errorf("load succeeded but the archive was not cleaned up")
	}
}

// A save archive without a readable image config must never cost the
// user their image: reclaim keeps the original and marks the entry HELD.
func TestReclaimHeldWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	log := filepath.Join(dir, "log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
case "$1" in
images) printf 'alpine:latest|34.2MB\n' ;;
save)
  d=$(mktemp -d)
  mkdir -p "$d/layerdir/etc"
  echo hello > "$d/layerdir/etc/motd"
  tar -cf "$d/layer1.tar" -C "$d/layerdir" etc
  mkdir -p "$d/layer1"
  mv "$d/layer1.tar" "$d/layer1/layer.tar"
  printf '[{"RepoTags":["%%s"],"Layers":["layer1/layer.tar"]}]' "$4" > "$d/manifest.json"
  tar -cf "$3" -C "$d" manifest.json layer1
  rm -rf "$d" ;;
rmi) echo "rmi $2" >> %s ;;
*) echo "unknown $1"; exit 1 ;;
esac`, log, log)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SQUISH_DOCKER", bin)
	t.Setenv("SQUISH_STORE", dir+"/store")

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	rcErr := cmdReclaim([]string{"--image", "alpine:latest"})
	w.Close()
	os.Stdout = oldStdout
	out, _ := io.ReadAll(r)
	if rcErr != nil {
		t.Fatalf("reclaim: %v", rcErr)
	}
	if !strings.Contains(string(out), "HELD alpine:latest") {
		t.Errorf("reclaim did not hold the unconfigurable image; stdout:\n%s", out)
	}
	l := readLog(t, log)
	if strings.Contains(l, "rmi alpine:latest") {
		t.Errorf("reclaim removed an image it cannot restore; log:\n%s", l)
	}
	st, err := openStore("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.LoadManifest("alpine_latest")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if m.ImageConfig != "" {
		t.Errorf("held manifest carries an image config hash %q", m.ImageConfig)
	}
}

// Run used to hand the whole argv to cmdRestore, so the subcommand word
// itself was treated as an image name. This is the CLI-level regression.
func TestRunRestoreDispatch(t *testing.T) {
	bin, log := writeFakeDocker(t)
	t.Setenv("SQUISH_DOCKER", bin)
	t.Setenv("SQUISH_STORE", t.TempDir()+"/store")
	if err := cmdReclaim(nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := Run([]string{"restore", "alpine:latest"}); err != nil {
		t.Fatalf("restore via Run: %v", err)
	}
	if !strings.Contains(readLog(t, log), "LOAD-OK") {
		t.Error("restore via Run did not reach docker load")
	}
}
