package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	cfs "kaze-sync/internal/client/fs"
)

// TestLatency measures how long one edit takes to reach the other client with the production timings
// (Stable 5 s, debounce 2 s, retry 10 s). It takes about a minute, so it runs only with KAZE_LATENCY=1;
// set TMPDIR to a tmpfs (/dev/shm) to keep it off the disk.
func TestLatency(t *testing.T) {
	t.Parallel()
	if os.Getenv("KAZE_LATENCY") == "" {
		t.Skip("set KAZE_LATENCY=1")
	}
	e := newCEnv(t)
	f := e.newFolder("One")
	var roots [2]string
	for i := range roots {
		dev, api := e.newDevice()
		roots[i] = filepath.Join(t.TempDir(), "one")
		e.mapFolder(dev, f, roots[i], true)
		a := &Agent{Dir: t.TempDir(), API: api, NewFS: cfs.OS, OS: runtime.GOOS, DeviceName: dev.Name,
			Stable: 5 * time.Second, ScanInterval: time.Hour, RetryAfter: 10 * time.Second}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	eventually(t, "both roots to be set up", func() bool {
		_, e1 := os.Stat(filepath.Join(roots[0], markerName))
		_, e2 := os.Stat(filepath.Join(roots[1], markerName))
		return e1 == nil && e2 == nil
	})
	time.Sleep(3 * time.Second)
	wait := func(root, p, want string, limit time.Duration) time.Duration {
		start := time.Now()
		for time.Since(start) < limit {
			if b, err := os.ReadFile(filepath.Join(root, p)); err == nil && string(b) == want {
				return time.Since(start)
			}
			time.Sleep(20 * time.Millisecond)
		}
		return -1
	}
	for i := 0; i < 4; i++ {
		p := "new" + itoa(int64(i)) + ".txt"
		writeFile(t, filepath.Join(roots[0], p), "v1")
		t.Logf("new file %d: %v", i, wait(roots[1], p, "v1", 40*time.Second))
		time.Sleep(time.Second)
	}
	for i := 0; i < 3; i++ {
		v := "edit" + itoa(int64(i))
		writeFile(t, filepath.Join(roots[1], "new0.txt"), v)
		t.Logf("edit %d (other direction): %v", i, wait(roots[0], "new0.txt", v, 40*time.Second))
		time.Sleep(time.Second)
	}
	for i := 0; i < 2; i++ { // the peer commits while the scan retry for our write is pending: its news arrives in between
		p, q := "race"+itoa(int64(i))+".txt", "peer"+itoa(int64(i))+".txt"
		writeFile(t, filepath.Join(roots[1], q), "p") // reaches the server about 12 s later (its own debounce and retry)
		time.Sleep(3 * time.Second)
		writeFile(t, filepath.Join(roots[0], p), "v1")
		t.Logf("write with a peer commit %d: %v (-1ns = not within 40 s)", i, wait(roots[1], p, "v1", 40*time.Second))
		time.Sleep(15 * time.Second)
	}
}
