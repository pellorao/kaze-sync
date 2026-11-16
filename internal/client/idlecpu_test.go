package client

import (
	"slices"
	"testing"
	"time"
)

// A file that is too fresh is retried by scanning its own directory only, and the record of it survives scans of
// other directories (idle CPU fix: the retry used to scan the whole folder after every edit).
func TestUnstableRetryScansOnlyTheDirectoryOfTheFreshFile(t *testing.T) {
	_, _, _, g := setup(t)
	g.f.Stable = 5 * time.Second
	now := time.Unix(0, 1_000_000_000_000)
	g.f.Now = func() time.Time { return now }
	g.put("other/old.txt", []byte("o"))
	g.put("d/sub/new.txt", []byte("n"))
	g.put("top.txt", []byte("t"))
	g.fs.SetMeta("other/old.txt", now.Add(-time.Minute).UnixNano(), false)
	g.fs.SetMeta("top.txt", now.Add(-time.Minute).UnixNano(), false)
	g.fs.SetMeta("d/sub/new.txt", now.Add(-time.Second).UnixNano(), false)
	g.scan()
	if got := g.f.UnstableDirs(); !slices.Equal(got, []string{"d/sub"}) {
		t.Fatalf("unstable dirs %v", got)
	}
	g.scanHint("other") // a scan of another directory does not forget the fresh file
	if got := g.f.UnstableDirs(); !slices.Equal(got, []string{"d/sub"}) {
		t.Fatalf("after a scan of another directory: %v", got)
	}
	g.scanHint("d/sub") // still too fresh
	if g.f.Unstable().IsZero() {
		t.Fatal("record lost while the file is still fresh")
	}
	now = now.Add(10 * time.Second)
	rep := g.scanHint("d/sub") // the retry: this directory only
	if rep.Committed == 0 || !g.f.Unstable().IsZero() || len(g.f.UnstableDirs()) != 0 {
		t.Fatalf("after the stable time: committed %d, unstable %v", rep.Committed, g.f.Unstable())
	}
}
