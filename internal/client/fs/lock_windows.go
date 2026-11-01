package fs

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// wrapLocked marks sharing and lock violations as ErrLocked.
func wrapLocked(err error) error {
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return fmt.Errorf("%w: %w", ErrLocked, err)
	}
	return err
}

// wrapRename also treats "access denied" as locked: replacing a file that another
// program holds open without FILE_SHARE_DELETE fails with it (a read-only target does too).
func wrapRename(err error) error {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("%w: %w", ErrLocked, err)
	}
	return wrapLocked(err)
}
