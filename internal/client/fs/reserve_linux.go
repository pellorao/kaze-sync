package fs

import (
	"errors"
	"os"
	"syscall"
)

// fallocKeepSize is FALLOC_FL_KEEP_SIZE: the space is allocated beyond the end of file without changing its length, so
// the length that OpenAppend reports for resuming stays the data written.
const fallocKeepSize = 1

func (o osFS) Reserve(p string, n int64) error {
	if err := o.guard(p); err != nil {
		return err
	}
	f, err := os.OpenFile(o.abs(p), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	err = syscall.Fallocate(int(f.Fd()), fallocKeepSize, fi.Size(), n)
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EFBIG):
		return ErrNoSpace
	case errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOSYS), errors.Is(err, syscall.EINVAL):
		return nil // this file system cannot reserve: the download fails when the disk fills, as before
	}
	return err
}
