package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// readInstalledVersion executes the opened root-owned binary, so renaming anything along path
// after the ownership check cannot substitute a different executable.
func readInstalledVersion(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if uid, _, ok := fileOwner(fi); !ok || uid != 0 || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
		return "", errors.New("installed kaze-syncd must be a root-owned regular file writable only by root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "--version")
	cmd.ExtraFiles = []*os.File{f}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(out)), binName+" ")
	if !ok {
		return "", errors.New("unexpected --version output")
	}
	return v, nil
}

// lockInstall serializes updates to an installation using its existing directory.
func lockInstall(data string) (*os.File, error) {
	f, err := os.Open(data)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock installation at %s: %w", data, err)
	}
	return f, nil
}

// SameDevice reports whether the nearest existing ancestor of p is on the
// same device as "/" (for OpenWrt: the overlay root file system).
func SameDevice(p string) (bool, error) {
	for {
		if _, err := os.Stat(p); err == nil {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	var a, b syscall.Stat_t
	if err := syscall.Stat(p, &a); err != nil {
		return false, err
	}
	if err := syscall.Stat("/", &b); err != nil {
		return false, err
	}
	return a.Dev == b.Dev, nil
}

// fileOwner returns the owner and group ids of a file as stat reported them.
func fileOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
