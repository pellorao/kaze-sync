package fs

import "golang.org/x/sys/windows"

// Reserve checks the free space of the volume: Windows cannot reserve space without changing the file's length,
// which OpenAppend's resume offset relies on.
func (o osFS) Reserve(p string, n int64) error {
	dir, err := windows.UTF16PtrFromString(o.root)
	if err != nil {
		return err
	}
	var avail uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &avail, nil, nil); err != nil {
		return nil // cannot tell: do not block the download
	}
	if avail < uint64(n) {
		return ErrNoSpace
	}
	return nil
}
