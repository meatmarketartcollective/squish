package cli

import (
	"os"
	"strings"
	"testing"
)

func failingLoadDocker(t *testing.T) (bin string, log string) {
	t.Helper()
	// the full fake, with load broken: reclaim succeeds, uninstall must not
	bin, log = writeFakeDocker(t)
	script, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	s = strings.Replace(s, "cfg=$(tar -xOf \"$f\" config.json)", "echo load refused; exit 1\n  cfg=$(tar -xOf \"$f\" config.json)", 1)
	if err := os.WriteFile(bin, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestUninstallRestoresAndRemovesStore(t *testing.T) {
	bin, log := writeFakeDocker(t)
	stRoot := t.TempDir() + "/store"
	t.Setenv("SQUISH_DOCKER", bin)
	t.Setenv("SQUISH_STORE", stRoot)
	if err := cmdReclaim(nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := cmdUninstall(nil); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(stRoot); !os.IsNotExist(err) {
		t.Errorf("store still exists after uninstall: %v", err)
	}
	l := readLog(t, log)
	if strings.Count(l, "LOAD-OK") != 2 {
		t.Errorf("expected both images loaded back, log:\n%s", l)
	}
}

func TestUninstallKeepsStoreWhenLoadFails(t *testing.T) {
	bin, _ := failingLoadDocker(t)
	stRoot := t.TempDir() + "/store"
	t.Setenv("SQUISH_DOCKER", bin)
	t.Setenv("SQUISH_STORE", stRoot)
	if err := cmdReclaim(nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	err := cmdUninstall(nil)
	if err == nil {
		t.Fatal("uninstall should fail when docker load refuses")
	}
	if _, serr := os.Stat(stRoot); serr != nil {
		t.Errorf("store must survive a failed uninstall: %v", serr)
	}
	m, lerr := openStore(stRoot)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if _, lerr := m.LoadManifest("alpine_latest"); lerr != nil {
		t.Errorf("manifest lost after failed uninstall: %v", lerr)
	}
}

func TestUninstallSkipsConfiglessManifest(t *testing.T) {
	bin, log := writeFakeDocker(t)
	stRoot := t.TempDir() + "/store"
	t.Setenv("SQUISH_DOCKER", bin)
	t.Setenv("SQUISH_STORE", stRoot)
	if err := cmdReclaim(nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	st, err := openStore(stRoot)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.LoadManifest("alpine_latest")
	if err != nil {
		t.Fatal(err)
	}
	m.ImageConfig = ""
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	if err := cmdUninstall(nil); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(stRoot); !os.IsNotExist(err) {
		t.Errorf("store still exists: %v", err)
	}
	l := readLog(t, log)
	if strings.Count(l, "LOAD-OK") != 1 {
		t.Errorf("only busybox should have been loaded, log:\n%s", l)
	}
}

func TestUninstallWithoutDockerKeepsStore(t *testing.T) {
	bin, _ := writeFakeDocker(t)
	stRoot := t.TempDir() + "/store"
	t.Setenv("SQUISH_DOCKER", bin)
	t.Setenv("SQUISH_STORE", stRoot)
	if err := cmdReclaim(nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	// docker gone (daemon down, binary removed)
	t.Setenv("SQUISH_DOCKER", "/bin/false")
	if err := cmdUninstall(nil); err == nil {
		t.Fatal("uninstall must refuse when docker is not usable")
	}
	if _, serr := os.Stat(stRoot); serr != nil {
		t.Errorf("store must survive: %v", serr)
	}
}
