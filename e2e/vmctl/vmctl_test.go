package vmctl

import (
	"os/exec"
	"testing"
	"time"
)

// A receiver that exits early must not leave the sender blocked on a full pipe.
func TestPipeTarReceiverExitsEarly(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- pipeTar(exec.Command("head", "-c", "10000000", "/dev/zero"), exec.Command("sh", "-c", "exit 1"))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a failed receiver was not reported")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pipeTar hangs when the receiver exits early")
	}
}

func TestPipeTarOK(t *testing.T) {
	if err := pipeTar(exec.Command("head", "-c", "1000000", "/dev/zero"), exec.Command("sh", "-c", "cat >/dev/null")); err != nil {
		t.Fatal(err)
	}
}
