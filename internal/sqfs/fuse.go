//go:build linux

package sqfs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"

	"github.com/aloysiusvonfck/squish/internal/store"
)

// fsNode is one path in the tree.
type fsNode struct {
	tree *Tree
	path string
}

// rootNode is the mount root; it behaves like a directory node for ".".
type rootNode struct {
	tree *Tree
}

func (r *rootNode) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (r *rootNode) Lookup(ctx context.Context, name string) (fusefs.Node, error) {
	return lookup(r.tree, name)
}

func (r *rootNode) ReadDir(ctx context.Context) ([]fuse.Dirent, error) {
	return readdir(r.tree, "")
}

func (n *fsNode) Attr(ctx context.Context, a *fuse.Attr) error {
	if e, ok := n.tree.File(n.path); ok {
		a.Size = uint64(e.Size)
		a.Mode = fileMode(e)
		return nil
	}
	if n.tree.IsDir(n.path) {
		a.Mode = os.ModeDir | 0o555
		return nil
	}
	return syscall.ENOENT
}

func fileMode(e store.FileEntry) os.FileMode {
	switch e.Kind {
	case store.KindSymlink:
		return os.ModeSymlink | 0o777
	case store.KindFile, store.KindFrame, store.KindDelta, store.KindCold:
		if e.Mode&0o111 != 0 {
			return os.FileMode(e.Mode) | 0o555
		}
		return os.FileMode(e.Mode) | 0o444
	default:
		return 0o444
	}
}

func lookup(tree *Tree, name string) (fusefs.Node, error) {
	p := name
	if tree.IsDir(p) {
		return &fsNode{tree: tree, path: p}, nil
	}
	if _, ok := tree.File(p); ok {
		return &fsNode{tree: tree, path: p}, nil
	}
	return nil, syscall.ENOENT
}

func readdir(tree *Tree, dir string) ([]fuse.Dirent, error) {
	names := tree.Children(dir)
	out := make([]fuse.Dirent, 0, len(names))
	for _, n := range names {
		var full string
		if dir == "" || dir == "." {
			full = n
		} else {
			full = dir + "/" + n
		}
		if tree.IsDir(full) {
			out = append(out, fuse.Dirent{Name: n, Type: fuse.DT_Dir})
			continue
		}
		if e, ok := tree.File(full); ok {
			out = append(out, fuse.Dirent{Name: n, Type: direntType(e)})
		}
	}
	return out, nil
}

func direntType(e store.FileEntry) fuse.DirentType {
	switch e.Kind {
	case store.KindSymlink:
		return fuse.DT_Link
	case store.KindDir:
		return fuse.DT_Dir
	default:
		return fuse.DT_File
	}
}

// fsFile is an open file handle serving chunked reads.
type fsFile struct {
	tree *Tree
	e    store.FileEntry
	data []byte
	read bool
}

func (n *fsNode) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fusefs.Handle, error) {
	e, ok := n.tree.File(n.path)
	if !ok {
		if n.tree.IsDir(n.path) {
			return n, nil // directories open as themselves for ReadDir
		}
		return nil, syscall.ENOENT
	}
	return &fsFile{tree: n.tree, e: e}, nil
}

func (f *fsFile) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	if !f.read {
		data, err := f.tree.ReadFile(&f.e)
		if err != nil {
			return err
		}
		f.data = data
		f.read = true
	}
	if req.Offset >= int64(len(f.data)) {
		resp.Data = resp.Data[:0]
		return nil
	}
	end := req.Offset + int64(req.Size)
	if end > int64(len(f.data)) {
		end = int64(len(f.data))
	}
	resp.Data = f.data[req.Offset:end]
	return nil
}

// treeFS adapts a Tree to the fuse fs.FS interface.
type treeFS struct {
	tree *Tree
}

func (t treeFS) Root() (fusefs.Node, error) { return &rootNode{tree: t.tree}, nil }

// Mount presents the manifest at mnt until it is unmounted.
func Mount(st *store.Store, m *store.Manifest, mnt string) error {
	if !fusermountAvailable() {
		return fmt.Errorf("mount: fusermount/fusermount3 not found; install fuse on this system to use squish mount")
	}
	tree := NewTree(st, m)
	conn, err := fuse.Mount(mnt, fuse.FSName("squish"), fuse.Subtype("squish"), fuse.ReadOnly())
	if err != nil {
		return fmt.Errorf("mount %s: %w", mnt, err)
	}
	srv := fusefs.New(conn, nil)
	if err := srv.Serve(treeFS{tree}); err != nil {
		_ = fuse.Unmount(mnt)
		return err
	}
	_ = fuse.Unmount(mnt)
	return nil
}

func fusermountAvailable() bool {
	for _, name := range []string{"fusermount3", "fusermount"} {
		if p, err := exec.LookPath(name); err == nil && p != "" {
			return true
		}
		for _, dir := range []string{"/usr/bin", "/usr/local/bin", "/bin", "/sbin", "/usr/sbin"} {
			p := dir + "/" + name
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return true
			}
		}
	}
	return false
}
