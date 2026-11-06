package sqfs

import (
	"context"
	"testing"

	"bazil.org/fuse"

	"github.com/aloysiusvonfck/squish/internal/store"
)

func TestReadlink(t *testing.T) {
	m := &store.Manifest{Name: "t"}
	m.Files = append(m.Files,
		store.FileEntry{Path: "etc", Kind: store.KindDir, Mode: 0o755},
		store.FileEntry{Path: "etc/host.conf", Kind: store.KindSymlink, Link: "../resolv.conf"},
	)
	tr := NewTree(nil, m)
	n := &fsNode{tree: tr, path: "etc/host.conf"}
	link, err := n.Readlink(context.Background(), nil)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if link != "../resolv.conf" {
		t.Errorf("readlink = %q", link)
	}
}

func TestNestedFuseTraversal(t *testing.T) {
	m := &store.Manifest{Name: "t"}
	m.Files = []store.FileEntry{
		{Path: "etc", Kind: store.KindDir, Mode: 0o755},
		{Path: "etc/motd", Size: 5, Mode: 0o644, Kind: store.KindFile, Hash: "deadbeef"},
		{Path: "etc/init.d", Kind: store.KindDir, Mode: 0o755},
		{Path: "etc/init.d/rcS", Size: 9, Mode: 0o755, Kind: store.KindFile, Hash: "cafebabe"},
	}
	tr := NewTree(nil, m)

	root := &rootNode{tree: tr}
	top, err := root.ReadDir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].Name != "etc" || top[0].Type != fuse.DT_Dir {
		t.Errorf("root listing = %+v", top)
	}

	etcNode, err := root.Lookup(context.Background(), "etc")
	if err != nil {
		t.Fatal(err)
	}
	etc, ok := etcNode.(*fsNode)
	if !ok {
		t.Fatal("etc lookup did not return an fsNode")
	}
	etcDir, err := etc.ReadDir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(etcDir) != 2 {
		t.Errorf("etc listing = %+v", etcDir)
	}

	nested, err := etc.Lookup(context.Background(), "init.d")
	if err != nil {
		t.Fatal(err)
	}
	deep, err := nested.(*fsNode).ReadDir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(deep) != 1 || deep[0].Name != "rcS" || deep[0].Type != fuse.DT_File {
		t.Errorf("init.d listing = %+v", deep)
	}
}
