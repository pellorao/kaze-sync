package watch

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The inotify limit is lowered inside a user namespace (unshare -r), where the
// test binary re-runs itself with a limit of 5 watches.
func TestAddFailsWhenWatchLimitExhausted(t *testing.T) {
	if os.Getenv("WATCH_LIMIT_CHILD") == "" {
		cmd := exec.Command("unshare", "-r", "sh", "-c",
			`echo 5 > /proc/sys/user/max_inotify_watches && exec "$0" -test.run='^TestAddFailsWhenWatchLimitExhausted$'`, os.Args[0])
		cmd.Env = append(os.Environ(), "WATCH_LIMIT_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			if strings.Contains(string(out), "FAIL") {
				t.Fatalf("child failed: %s", out)
			}
			t.Skipf("cannot lower the inotify limit here: %v: %s", err, out)
		}
		return
	}
	root := t.TempDir()
	out := make(chan string, 100)
	w, err := Start(root, out)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 20; i++ {
		d := fmt.Sprintf("d%d", i)
		mkdirs(t, root, d)
		if err := w.Add(d); err != nil {
			t.Logf("Add failed after %d directories: %v", i, err)
			return
		}
	}
	t.Fatal("Add never failed although the limit is 5 watches")
}
