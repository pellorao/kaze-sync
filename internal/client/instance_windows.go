package client

import (
	"os"

	"golang.org/x/sys/windows"
)

// The locked byte lies far beyond the pid at offset 0: Windows byte-range locks are mandatory, and StopAgent
// must be able to read the pid of the agent that holds the lock.
func lockRange() *windows.Overlapped { return &windows.Overlapped{Offset: 1 << 30} }

func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, lockRange())
}

func unlockFile(f *os.File) { windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockRange()) }
