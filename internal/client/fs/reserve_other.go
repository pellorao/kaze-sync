//go:build !linux && !windows

package fs

func (o osFS) Reserve(p string, n int64) error { return nil }
