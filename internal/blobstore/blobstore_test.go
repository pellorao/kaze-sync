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
