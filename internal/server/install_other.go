//go:build !linux

package server

import "errors"

func SameDevice(string) (bool, error) {
	return false, errors.New("install is only supported on Linux")
}
