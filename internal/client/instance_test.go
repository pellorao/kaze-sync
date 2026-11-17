package client

import (
	"errors"
	"testing"
)

func TestSingleInstance(t *testing.T) {
	dir := t.TempDir()
	release, err := LockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockInstance(dir); !errors.Is(err, ErrAgentRunning) {
		t.Fatalf("second lock: %v", err)
	}
	release()
	again, err := LockInstance(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}

func TestStopAgentWithoutAgent(t *testing.T) {
	dir := t.TempDir()
	if err := StopAgent(dir); err != nil {
		t.Fatal(err)
	}
	release, _ := LockInstance(dir)
	release()
	if err := StopAgent(dir); err != nil { // a stale pid in the file, nobody holds the lock
		t.Fatal(err)
	}
}
