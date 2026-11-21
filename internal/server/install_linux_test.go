package server

import (
	"syscall"
	"testing"
	"time"
)

// A forked child holds a copy of the lock's descriptor until it execs, so a lock that was just closed can still
// look held. The copy is simulated with a dup that is closed after a short while.
func TestLockInstallWaitsForACopyOfTheDescriptor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := lockInstall(dir)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Dup(int(l.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	go func() { time.Sleep(30 * time.Millisecond); syscall.Close(fd) }()
	l2, err := lockInstall(dir)
	if err != nil {
		t.Fatalf("lock after a close with a lingering copy: %v", err)
	}
	l2.Close()
}
