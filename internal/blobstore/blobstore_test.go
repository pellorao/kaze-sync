package blobstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func hashOf(t *testing.T, data []byte) Hash {
	t.Helper()
	h, n, err := HashReader(bytes.NewReader(data))
	if err != nil || n != int64(len(data)) {
		t.Fatal(h, n, err)
	}
	return h
}

func TestParseHash(t *testing.T) {
	h := hashOf(t, []byte("x"))
	if got, err := ParseHash(h.String()); err != nil || got != h {
		t.Fatalf("round trip: %v %v", got, err)
	}
	for _, s := range []string{"", "zz", strings.ToUpper(h.String()), h.String()[2:], h.String() + "00"} {
		if _, err := ParseHash(s); err == nil {
			t.Errorf("ParseHash(%q) succeeded", s)
		}
	}
}

func put(t *testing.T, s *Store, data []byte) Hash {
	t.Helper()
	h := hashOf(t, data)
	if err := s.Put(h, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return h
}

func tmpCount(s *Store) int {
	entries, _ := os.ReadDir(filepath.Join(s.dir, "tmp"))
	return len(entries)
}

func TestPutRoundTrip(t *testing.T) {
	s := newStore(t)
	data := []byte("hello, content-addressed world")
	h := hashOf(t, data)
	if ok, err := s.Has(h); ok || err != nil {
		t.Fatalf("Has before Put = %v %v", ok, err)
	}
	put(t, s, data)
	if ok, err := s.Has(h); !ok || err != nil {
		t.Fatalf("Has = %v %v", ok, err)
	}
	f, err := s.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got, _ := io.ReadAll(f); !bytes.Equal(got, data) {
		t.Errorf("content = %q", got)
	}
	if fi, _ := f.Stat(); fi.Mode().Perm() != 0o444 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if n := tmpCount(s); n != 0 {
		t.Errorf("%d temp files left", n)
	}
}

func TestPutHashMismatch(t *testing.T) {
	s := newStore(t)
	h := hashOf(t, []byte("expected"))
	if err := s.Put(h, strings.NewReader("tampered")); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("err = %v", err)
	}
	if ok, _ := s.Has(h); ok {
		t.Error("blob stored")
	}
	if n := tmpCount(s); n != 0 {
		t.Errorf("%d temp files left", n)
	}
}

func TestConcurrentPutOfSameHash(t *testing.T) {
	s := newStore(t)
	data := bytes.Repeat([]byte("z"), 1<<16)
	h := hashOf(t, data)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Put(h, bytes.NewReader(data))
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Errorf("Put: %v", err)
		}
	}
	if ok, _ := s.Has(h); !ok {
		t.Error("no blob after concurrent Puts")
	}
	if n := tmpCount(s); n != 0 {
		t.Errorf("%d temp files left", n)
	}
	var blobs int
	filepath.WalkDir(filepath.Join(s.dir, "blobs"), func(_ string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			blobs++
		}
		return nil
	})
	if blobs != 1 {
		t.Errorf("%d stored copies", blobs)
	}
}

func TestSweep(t *testing.T) {
	s := newStore(t)
	putAged := func(content string, age time.Duration) Hash {
		h := put(t, s, []byte(content))
		old := time.Now().Add(-age)
		os.Chtimes(s.blobPath(h), old, old)
		return h
	}
	kept := putAged("referenced and old", 48*time.Hour)
	doomed := putAged("unreferenced and old", 48*time.Hour)
	fresh := putAged("unreferenced but new", time.Minute)

	// A Put interrupted by a reader error leaves nothing; simulate a crash with stray temp files.
	tmp := filepath.Join(s.dir, "tmp")
	oldTmp, newTmp := filepath.Join(tmp, "put-old"), filepath.Join(tmp, "put-new")
	os.WriteFile(oldTmp, []byte("p"), 0o644)
	os.WriteFile(newTmp, []byte("p"), 0o644)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(oldTmp, old, old)

	// Two ranges that together cover the hash space sweep everything once.
	keep := func(h Hash) bool { return h == kept }
	n1, err1 := s.SweepBlobs(0, 128, keep, time.Hour)
	n2, err2 := s.SweepBlobs(128, 256, keep, time.Hour)
	nt, err3 := s.SweepTmp(24 * time.Hour)
	if err1 != nil || err2 != nil || err3 != nil || n1+n2 != 1 || nt != 1 {
		t.Fatalf("SweepBlobs = %d, %d (%v, %v), SweepTmp = %d (%v)", n1, n2, err1, err2, nt, err3)
	}
	if n1 == 1 && doomed[0] >= 128 || n2 == 1 && doomed[0] < 128 {
		t.Fatal("a range swept a blob outside it")
	}
	for h, want := range map[Hash]bool{kept: true, doomed: false, fresh: true} {
		if ok, _ := s.Has(h); ok != want {
			t.Errorf("blob %s present=%v, want %v", h, ok, want)
		}
	}
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Error("old temp not removed")
	}
	if _, err := os.Stat(newTmp); err != nil {
		t.Error("new temp removed")
	}
}

func ageBlob(t *testing.T, s *Store, h Hash, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(s.blobPath(h), old, old); err != nil {
		t.Fatal(err)
	}
}

// Use renews an old block, so a sweep that judged it unreferenced earlier keeps it when it looks again;
// a young block keeps its time; a missing block is reported as such.
func TestUseRenewsOldBlocks(t *testing.T) {
	s := newStore(t)
	h := put(t, s, []byte("block"))
	young, _ := os.Stat(s.blobPath(h))
	if ok, err := s.Use(h); !ok || err != nil {
		t.Fatalf("Use: %v %v", ok, err)
	}
	if fi, _ := os.Stat(s.blobPath(h)); !fi.ModTime().Equal(young.ModTime()) {
		t.Error("a young block was touched")
	}
	ageBlob(t, s, h, 2*time.Hour)
	if ok, err := s.Use(h); !ok || err != nil {
		t.Fatalf("Use old: %v %v", ok, err)
	}
	if n, err := s.SweepBlobs(0, 256, func(Hash) bool { return false }, time.Hour); n != 0 || err != nil {
		t.Fatalf("a used block was swept: %d %v", n, err)
	}
	if ok, err := s.Use(hashOf(t, []byte("absent"))); ok || err != nil {
		t.Fatalf("Use missing: %v %v", ok, err)
	}
	ageBlob(t, s, h, 2*time.Hour)
	if n, _ := s.SweepBlobs(0, 256, func(Hash) bool { return false }, time.Hour); n != 1 {
		t.Fatalf("an unused old block stayed: %d", n)
	}
}

// Whatever the timing, a block that Put and Use keep storing and renewing is never swept while they
// run, and the store stays usable.
func TestSweepRacesWithPutAndUse(t *testing.T) {
	s := newStore(t)
	data := []byte("contended block")
	h := hashOf(t, data)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			s.Put(h, bytes.NewReader(data))
			old := time.Now().Add(-2 * time.Hour) // as if it had been stored long ago, then re-referenced
			os.Chtimes(s.blobPath(h), old, old)
			if ok, _ := s.Use(h); !ok {
				continue // swept before Use: a commit would ask for the block again
			}
			if _, err := s.Open(h); err != nil {
				t.Errorf("block vanished right after Use: %v", err)
				return
			}
		}
	}()
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			s.SweepBlobs(0, 256, func(Hash) bool { return false }, time.Hour)
		}
	}
}

func TestOpenRefusesSymbolicLinks(t *testing.T) {
	dir, victim := t.TempDir(), t.TempDir()
	os.Symlink(victim, filepath.Join(dir, "tmp"))
	if _, err := Open(dir); err == nil {
		t.Fatal("Open followed a symbolic link")
	}
	if entries, _ := os.ReadDir(victim); len(entries) != 0 {
		t.Fatalf("the link target was written to: %v", entries)
	}
}

func TestWriterStoresEveryBlockByWait(t *testing.T) {
	s := newStore(t)
	w := s.NewWriter()
	var hs []Hash
	for i := 0; i < 100; i++ { // more than the writer has in flight
		data := []byte(strings.Repeat("block ", i+1))
		h := hashOf(t, data)
		hs = append(hs, h)
		w.Put(h, data)
	}
	w.Put(hashOf(t, []byte("a")), []byte("b"))
	if err := w.Wait(); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	for _, h := range hs {
		if ok, _ := s.Has(h); !ok {
			t.Errorf("%s missing after Wait", h)
		}
	}
	if n := tmpCount(s); n != 0 {
		t.Errorf("%d temporary files left", n)
	}
}

// A stored blob's directory entry is flushed (rename alone is not durable), and so are the fan-out directories
// when they were just created.
func TestPutSyncsTheDirectories(t *testing.T) {
	var synced []string
	old := syncDir
	syncDir = func(d string) error { synced = append(synced, d); return nil }
	defer func() { syncDir = old }()
	s := newStore(t)
	data := []byte("durable")
	h := hashOf(t, data)
	put(t, s, data)
	want := []string{filepath.Dir(s.blobPath(h)), filepath.Dir(filepath.Dir(s.blobPath(h))), filepath.Join(s.dir, "blobs")}
	if strings.Join(synced, "|") != strings.Join(want, "|") {
		t.Fatalf("first put synced %v, want %v", synced, want)
	}
	synced = nil
	put(t, s, data) // the same blob again: the directories exist
	if len(synced) != 1 || synced[0] != want[0] {
		t.Fatalf("second put synced %v", synced)
	}
}
