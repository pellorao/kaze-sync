//go:build !windows

package fs

func wrapLocked(err error) error { return err }

func wrapRename(err error) error { return err }
