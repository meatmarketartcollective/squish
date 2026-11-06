package sqfs

import (
	"context"
	"testing"

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
