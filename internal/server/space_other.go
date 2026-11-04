//go:build !linux

package server

func freeBytes(dir string) (uint64, bool) { return 0, false }
