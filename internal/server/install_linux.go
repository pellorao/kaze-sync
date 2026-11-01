package server

import (
	"os"
	"path/filepath"
	"syscall"
)

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
