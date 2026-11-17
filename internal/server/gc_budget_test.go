package server

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"kaze-sync/internal/blobstore"
)

func memoryFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadMemTotal(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       int64
	}{
		{"512MB", "MemFree: 123 kB\nMemTotal:       524288 kB\n", 512 << 20},
		{"missing field", "MemFree: 123 kB\n", 0},
		{"empty", "", 0},
		{"bad number", "MemTotal: invalid kB\n", 0},
		{"negative", "MemTotal: -1 kB\n", 0},
		{"zero", "MemTotal: 0 kB\n", 0},
		{"wrong unit", "MemTotal: 512 MB\n", 0},
		{"no unit", "MemTotal: 524288\n", 0},
		{"overflow", "MemTotal: 9223372036854775807 kB\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readMemTotal(memoryFile(t, "meminfo", tc.text)); got != tc.want {
				t.Fatalf("readMemTotal = %d, want %d", got, tc.want)
			}
		})
	}
	if got := readMemTotal(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Fatalf("missing meminfo = %d, want 0", got)
	}
}

func TestReadMemoryLimit(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       int64
	}{
		{"v2", "536870912\n", 512 << 20},
		{"v1", "268435456\n", 256 << 20},
		{"max", "max\n", -1},
		{"v1 unlimited", "9223372036854771712\n", -1},
		{"unsigned unlimited", "18446744073709551615\n", -1},
		{"empty", "", -1},
		{"bad number", "invalid", -1},
		{"negative", "-1\n", -1},
		{"zero", "0\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readMemoryLimit(memoryFile(t, "limit", tc.text)); got != tc.want {
				t.Fatalf("readMemoryLimit = %d, want %d", got, tc.want)
			}
		})
	}
	if got := readMemoryLimit(filepath.Join(t.TempDir(), "missing")); got != -1 {
		t.Fatalf("missing cgroup limit = %d, want -1", got)
	}
}

func TestGCMemoryLimitPaths(t *testing.T) {
	root := t.TempDir()
	defaults := []string{filepath.Join(root, "memory.max"), filepath.Join(root, "memory", "memory.limit_in_bytes")}
	for _, tc := range []struct {
		name, membership string
		extra            []string
	}{
		{"v2 root", "0::/\n", nil},
		{"v2 nested", "0::/system.slice/sync.service\n", []string{
			filepath.Join(root, "system.slice", "sync.service", "memory.max"),
			filepath.Join(root, "system.slice", "memory.max"),
		}},
		{"v1 nested", "2:cpu,cpuacct:/other\n3:memory,other:/services/sync\n", []string{
			filepath.Join(root, "memory", "services", "sync", "memory.limit_in_bytes"),
			filepath.Join(root, "memory", "services", "memory.limit_in_bytes"),
		}},
		{"malformed", "bad line\n2:cpu:/other\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gcMemoryLimitPaths(memoryFile(t, "cgroup", tc.membership), root)
			want := append(slices.Clone(defaults), tc.extra...)
			if !slices.Equal(got, want) {
				t.Fatalf("limit paths = %v, want %v", got, want)
			}
		})
	}
	if got := gcMemoryLimitPaths(filepath.Join(t.TempDir(), "missing"), root); !slices.Equal(got, defaults) {
		t.Fatalf("missing membership: %v, want %v", got, defaults)
	}
	// An unlimited child still inherits its parent's finite memory limit.
	if err := os.MkdirAll(filepath.Join(root, "parent", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(root, "memory.max"):                    "max",
		filepath.Join(root, "parent", "memory.max"):          "536870912",
		filepath.Join(root, "parent", "child", "memory.max"): "max",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths := gcMemoryLimitPaths(memoryFile(t, "cgroup", "0::/parent/child\n"), root)
	if got := gcMemoryBudget(memoryFile(t, "meminfo", "MemTotal: 1048576 kB\n"), paths...); got != 192<<20 {
		t.Fatalf("inherited budget = %d, want %d", got, 192<<20)
	}
}

func TestGCMemoryBudget(t *testing.T) {
	for _, tc := range []struct {
		name, meminfo, v2, v1 string
		want                  int64
	}{
		{"512MB", "MemTotal: 524288 kB\n", "", "", 192 << 20},
		{"256MB", "MemTotal: 262144 kB\n", "", "", 96 << 20},
		{"1GB", "MemTotal: 1048576 kB\n", "", "", 384 << 20},
		{"floor", "MemTotal: 131072 kB\n", "", "", 64 << 20},
		{"v2 smaller", "MemTotal: 1048576 kB\n", "536870912", "", 192 << 20},
		{"v1 smaller", "MemTotal: 1048576 kB\n", "", "268435456", 96 << 20},
		{"host smaller", "MemTotal: 524288 kB\n", "1073741824", "", 192 << 20},
		{"both limits", "MemTotal: 1048576 kB\n", "536870912", "268435456", 96 << 20},
		{"max", "MemTotal: 524288 kB\n", "max", "", 192 << 20},
		{"max with v1", "MemTotal: 1048576 kB\n", "max", "536870912", 192 << 20},
		{"v1 unlimited", "MemTotal: 524288 kB\n", "", "9223372036854771712", 192 << 20},
		{"limit floor", "MemTotal: 1048576 kB\n", "67108864", "", 64 << 20},
		{"zero limit floor", "MemTotal: 1048576 kB\n", "0", "", 64 << 20},
		{"odd bytes", "MemTotal: 1048576 kB\n", "268435459", "", 100663297},
		{"all missing", "", "", "", 64 << 20},
		{"meminfo missing", "", "536870912", "", 64 << 20},
		{"meminfo invalid", "MemTotal: invalid kB\n", "536870912", "", 64 << 20},
		{"limits invalid", "MemTotal: 524288 kB\n", "invalid", "-1", 192 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := [3]string{}
			for i, content := range []string{tc.meminfo, tc.v2, tc.v1} {
				paths[i] = filepath.Join(t.TempDir(), "missing")
				if content != "" {
					paths[i] = memoryFile(t, "memory", content)
				}
			}
			if got := gcMemoryBudget(paths[0], paths[1], paths[2]); got != tc.want {
				t.Fatalf("gcMemoryBudget = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGCMemoryBudgetLog(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "meta.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(db, dir, blobs, time.Now)
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	defer slog.SetDefault(old)
	s.requestGC()
	s.gcWG.Wait()
	if !bytes.Contains(log.Bytes(), []byte("msg=gc ")) || !bytes.Contains(log.Bytes(), []byte(fmt.Sprintf(" budget=%d ", GCMemoryBudget))) {
		t.Fatalf("GC log lacks budget in bytes: %s", log.String())
	}
}

// A cgroup path is always POSIX; walking its ancestors must end on any host OS
// (filepath.Dir on Windows never reaches "/").
func TestGCMemoryLimitPathsTerminates(t *testing.T) {
	dir := t.TempDir()
	m := filepath.Join(dir, "cgroup")
	if err := os.WriteFile(m, []byte("0::/a/b/c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan []string, 1)
	go func() { done <- gcMemoryLimitPaths(m, dir) }()
	select {
	case paths := <-done:
		if len(paths) != 2+3 {
			t.Fatalf("paths %v", paths)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gcMemoryLimitPaths does not terminate")
	}
}
