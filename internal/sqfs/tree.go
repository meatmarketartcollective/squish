// Package sqfs presents a squish manifest as a read-only filesystem
// tree: pure path-tree logic plus a FUSE adapter for `squish mount`.
package sqfs

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/aloysiusvonfck/squish/internal/store"
)

// Tree indexes a manifest as a path tree over a store.
type Tree struct {
	st    *store.Store
	m     *store.Manifest
	files map[string]store.FileEntry // full path -> entry
	dirs  map[string]bool
}

// NewTree builds the path index from the manifest.
func NewTree(st *store.Store, m *store.Manifest) *Tree {
	t := &Tree{st: st, m: m, files: map[string]store.FileEntry{}, dirs: map[string]bool{}}
	for _, e := range m.Files {
		p := strings.TrimPrefix(path.Clean("/"+e.Path), "/")
		switch e.Kind {
		case store.KindDir:
			t.dirs[p] = true
		case store.KindFile, store.KindChunked, store.KindFrame, store.KindDelta, store.KindCold, store.KindSymlink, store.KindLink, store.KindElided, store.KindSpecial:
			t.files[p] = e
			for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
				t.dirs[d] = true
			}
		}
	}
	return t
}

// IsDir reports whether the path is a directory in the tree.
func (t *Tree) IsDir(p string) bool { return t.dirs[p] }

// File returns the entry for a file path, if any.
func (t *Tree) File(p string) (store.FileEntry, bool) {
	e, ok := t.files[p]
	return e, ok
}

// Children lists the immediate children of a directory path, sorted,
// directories first.
func (t *Tree) Children(dir string) []string {
	if dir == "" {
		dir = "."
	}
	seen := map[string]bool{}
	var out []string
	// directories are prefixes: collect the next path component
	for d := range t.dirs {
		if c, ok := childOf(dir, d); ok && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for f := range t.files {
		if c, ok := childOf(dir, f); ok && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// childOf returns the next component of full when it lies under dir,
// else ok=false.
func childOf(dir, full string) (string, bool) {
	if dir == "." {
		dir = ""
	}
	full = strings.TrimSuffix(full, "/")
	if dir != "" && !strings.HasPrefix(full, dir+"/") {
		return "", false
	}
	rest := full
	if dir != "" {
		rest = full[len(dir)+1:]
	}
	if rest == "" {
		return "", false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest, true
}

// ReadFile returns the contents of a file entry by its kind.
func (t *Tree) ReadFile(e *store.FileEntry) ([]byte, error) {
	switch e.Kind {
	case store.KindFile:
		return t.st.ReadBlob(e.Hash)
	case store.KindFrame:
		return t.st.ReadFrameFile(e.Frame, e.Offset, e.Clen)
	case store.KindDelta:
		return t.st.ReadDelta(e.Hash, e.Base)
	case store.KindCold:
		return t.st.ReadColdEntry(e)
	case store.KindChunked:
		return t.st.ReadChunked(e)
	case store.KindLink:
		// A hardlink may point at another hardlink; follow the chain to
		// its ultimate file and refuse cycles.
		seen := map[string]bool{strings.TrimPrefix(path.Clean("/"+e.Path), "/"): true}
		cur := *e
		for cur.Kind == store.KindLink {
			p := strings.TrimPrefix(path.Clean("/"+cur.Link), "/")
			if seen[p] {
				return nil, fmt.Errorf("%s: hardlink chain never reaches a file", e.Path)
			}
			seen[p] = true
			tgt, ok := t.files[p]
			if !ok {
				return nil, fmt.Errorf("%s: hardlink target %q is not in this image", e.Path, cur.Link)
			}
			cur = tgt
		}
		return t.ReadFile(&cur)
	default:
		return nil, fmt.Errorf("%s: kind %q has no readable contents", e.Path, e.Kind)
	}
}
