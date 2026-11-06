package main

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func main() {
	outside, _ := os.MkdirTemp("", "out")
	base, _ := os.MkdirTemp("", "in")
	if err := os.Symlink(outside, base+"/link"); err != nil { panic(err) }
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil { panic(err) }
	_, e1 := unix.Openat(fd, "link", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	fmt.Println("openat O_NOFOLLOW on symlink-to-dir:", e1)
	var st unix.Stat_t
	e2 := unix.Fstatat(fd, "link", &st, unix.AT_SYMLINK_NOFOLLOW)
	fmt.Println("fstatat AT_SYMLINK_NOFOLLOW:", e2, "mode:", st.Mode&unix.S_IFMT == unix.S_IFLNK)
	e3 := unix.Mkdirat(fd, "via-link", 0o755)
	fmt.Println("mkdirat on symlink dirfd (should still target base):", e3)
	_ = syscall.Unlink(base + "/link")
	_ = os.RemoveAll(base)
	_ = os.RemoveAll(outside)
}
