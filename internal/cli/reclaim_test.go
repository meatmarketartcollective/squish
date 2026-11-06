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
  printf '{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/bin"]}}' > "$d/config.json"
  mkdir -p "$d/layerdir/etc"
  echo hello > "$d/layerdir/etc/motd"
  printf 'shared content' > "$d/layerdir/etc/shared.txt"
  ln "$d/layerdir/etc/shared.txt" "$d/layerdir/etc/shared-link.txt"
  tar -cf "$d/layer1.tar" -C "$d/layerdir" etc
  mkdir -p "$d/layer1"
  mv "$d/layer1.tar" "$d/layer1/layer.tar"
  printf '[{"Config":"config.json","RepoTags":["%%s"],"Layers":["layer1/layer.tar"]}]' "$4" > "$d/manifest.json"
  tar -cf "$3" -C "$d" manifest.json config.json layer1
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
