//go:build linux

package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/aloysiusvonfck/squish/internal/store"
)

// materializeSecure performs every operation below the extraction root relative
// to an already-open directory descriptor. O_NOFOLLOW on every component
// prevents a manifest-created symlink (or a concurrent replacement) from
// redirecting writes outside the root.
func materializeSecure(st *store.Store, m *store.Manifest, out string, useHardlinks, verifyEach bool) (files int, err error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return 0, err
	}
	rootPath, err := filepath.EvalSymlinks(out)
	if err != nil {
		return 0, err
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return 0, err
	}
	root, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, fmt.Errorf("open materialization root: %w", err)
	}
	defer unix.Close(root)

	first := map[string]string{}
	written := map[string]string{}
	byPath := map[string]store.FileEntry{}
	var links []store.FileEntry

	for _, e := range m.Files {
		if _, err := safeJoin(rootPath, e.Path); err != nil {
			return files, err
		}
		switch e.Kind {
		case store.KindDir:
			if err := secureMkdirAll(root, e.Path, os.FileMode(e.Mode)|0o700); err != nil {
				return files, fmt.Errorf("%s: %w", e.Path, err)
			}
		case store.KindFile, store.KindFrame, store.KindDelta, store.KindCold:
			parent, base, err := secureParent(root, e.Path, true)
			if err != nil {
				return files, fmt.Errorf("%s: %w", e.Path, err)
			}
			data, err := readEntryData(st, e)
			if err != nil {
				unix.Close(parent)
				return files, fmt.Errorf("%s: %w", e.Path, err)
			}
			if int64(len(data)) != e.Size {
				unix.Close(parent)
				return files, fmt.Errorf("%s: size mismatch (%d != %d)", e.Path, len(data), e.Size)
			}
			if err := secureWriteAt(parent, base, data, os.FileMode(e.Mode)); err != nil {
				unix.Close(parent)
				return files, fmt.Errorf("%s: %w", e.Path, err)
			}
			unix.Close(parent)
			if prev, ok := first[e.Hash]; !ok {
				first[e.Hash] = e.Path
			} else if useHardlinks {
				// Dedup hardlinks are an optimization. If the filesystem refuses
				// them, the already-written copy remains valid.
				_ = secureLink(root, prev, e.Path)
			}
			written[e.Path] = e.Path
			byPath[e.Path] = e
			if verifyEach {
				got, err := secureRead(root, e.Path)
				if err != nil {
					return files, err
				}
				if store.HashBytes(got) != e.Hash {
					return files, fmt.Errorf("%s: hash mismatch after materialize", e.Path)
				}
			}
			files++
		case store.KindSymlink:
			parent, base, err := secureParent(root, e.Path, true)
			if err != nil {
				return files, fmt.Errorf("%s: %w", e.Path, err)
			}
			_ = unix.Unlinkat(parent, base, 0)
			err = unix.Symlinkat(e.Link, parent, base)
			unix.Close(parent)
			if err != nil {
				return files, fmt.Errorf("%s: create symlink: %w", e.Path, err)
			}
			written[e.Path] = e.Path
		case store.KindLink:
			links = append(links, e)
		case store.KindElided, store.KindSpecial:
			// Intentionally omitted.
		}
	}

	linkTarget := make(map[string]string, len(links))
	for _, le := range links {
		linkTarget[le.Path] = le.Link
	}
	resolveBase := func(path string) (string, bool) {
		seen := map[string]bool{}
		for {
			if seen[path] {
				return "", false
			}
			seen[path] = true
			if target, ok := linkTarget[path]; ok {
				path = target
				continue
			}
			return path, true
		}
	}
	for _, le := range links {
		base, ok := resolveBase(le.Link)
		if !ok {
			return files, fmt.Errorf("%s: hardlink chain through %q never reaches a file", le.Path, le.Link)
		}
		if _, ok := written[base]; !ok {
			return files, fmt.Errorf("%s: hardlink target %q was not materialized", le.Path, base)
		}
		if err := secureLink(root, base, le.Path); err != nil {
			// Some filesystems do not support hardlinks. Copy the ultimate
			// regular-file payload instead, still using descriptor-relative I/O.
			entry, exists := byPath[base]
			if !exists {
				return files, fmt.Errorf("%s: hardlink target %q is not a regular stored file: %w", le.Path, base, err)
			}
			data, rerr := readEntryData(st, entry)
			if rerr != nil {
				return files, rerr
			}
			parent, name, perr := secureParent(root, le.Path, true)
			if perr != nil {
				return files, perr
			}
			perr = secureWriteAt(parent, name, data, os.FileMode(le.Mode))
			unix.Close(parent)
			if perr != nil {
				return files, perr
			}
		}
		files++
	}
	return files, nil
}

func secureParts(rel string) ([]string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "/") {
		return nil, fmt.Errorf("invalid relative path %q", rel)
	}
	parts := strings.Split(clean, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("unsafe path component in %q", rel)
		}
	}
	return parts, nil
}

// secureParent opens (and optionally creates) each parent component without
// following symlinks. It returns an owned descriptor for the final parent.
func secureParent(root int, rel string, create bool) (int, string, error) {
	parts, err := secureParts(rel)
	if err != nil {
		return -1, "", err
	}
	fd, err := unix.Dup(root)
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if e == unix.ENOENT && create {
			if e = unix.Mkdirat(fd, part, 0o755); e == nil || e == unix.EEXIST {
				next, e = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			}
		}
		unix.Close(fd)
		if e != nil {
			return -1, "", fmt.Errorf("unsafe or inaccessible parent %q: %w", part, e)
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func secureMkdirAll(root int, rel string, mode os.FileMode) error {
	parts, err := secureParts(rel)
	if err != nil {
		return err
	}
	fd, err := unix.Dup(root)
	if err != nil {
		return err
	}
	defer func() { if fd >= 0 { unix.Close(fd) } }()
	for _, part := range parts {
		e := unix.Mkdirat(fd, part, uint32(mode.Perm()))
		if e != nil && e != unix.EEXIST {
			return e
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if e != nil {
			return fmt.Errorf("directory component %q is not a real directory: %w", part, e)
		}
		unix.Close(fd)
		fd = next
	}
	return nil
}

func secureWriteAt(parent int, name string, data []byte, mode os.FileMode) error {
	if err := unix.Unlinkat(parent, name, 0); err != nil && err != unix.ENOENT {
		return err
	}
	fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	for len(data) > 0 {
		n, e := unix.Write(fd, data)
		if e == unix.EINTR {
			continue
		}
		if e != nil {
			return e
		}
		if n == 0 {
			return fmt.Errorf("short write")
		}
		data = data[n:]
	}
	return nil
}

func secureRead(root int, rel string) ([]byte, error) {
	parent, name, err := secureParent(root, rel, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var out []byte
	buf := make([]byte, 64*1024)
	for {
		n, e := unix.Read(fd, buf)
		if e == unix.EINTR {
			continue
		}
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if e != nil {
			return nil, e
		}
		if n == 0 {
			break
		}
	}
	return out, nil
}

func secureLink(root int, source, dest string) error {
	srcFD, srcName, err := secureParent(root, source, false)
	if err != nil {
		return err
	}
	defer unix.Close(srcFD)
	dstFD, dstName, err := secureParent(root, dest, true)
	if err != nil {
		return err
	}
	defer unix.Close(dstFD)
	if err := unix.Unlinkat(dstFD, dstName, 0); err != nil && err != unix.ENOENT {
		return err
	}
	return unix.Linkat(srcFD, srcName, dstFD, dstName, 0)
}
