package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// With a tiny buffer and a consumer that is not reading, the kernel buffer
// overflows and the watcher reports "" (rescan everything).
func TestOverflow(t *testing.T) {
	old := bufSize
	bufSize = 64
	defer func() { bufSize = old }()
	root := t.TempDir()
	out := make(chan string) // unbuffered: the watcher blocks on the first event
	w, err := Start(root, out)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 200; i++ {
		os.WriteFile(filepath.Join(root, strings.Repeat("x", 40)+string(rune('a'+i%26))+string(rune('a'+i/26))), nil, 0o644)
	}
	timeout := time.After(2 * time.Second)
	for {
		select {
		case got := <-out:
			if got == "" {
				return
			}
		case <-timeout:
			t.Fatal("no overflow notification within 2s")
		}
	}
}
