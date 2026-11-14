//go:build !linux

package server

import (
	"errors"
	"os"
)

func SameDevice(string) (bool, error) {
	return false, errors.New("install is only supported on Linux")
}

func fileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
