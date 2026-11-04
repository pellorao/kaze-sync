package place

import (
	"errors"
	"io"
	"strings"
	"testing"

	"kaze-sync/internal/blobstore"
	cfs "kaze-sync/internal/client/fs"
)

const content = "the quick brown fox jumps over the lazy dog"

var hash = func() blobstore.Hash {
	h, _, _ := blobstore.HashReader(strings.NewReader(content))
	return h
}()

func put(t *testing.T, f cfs.FS, p, data string) cfs.Entry {
	t.Helper()
	w, _, err := f.OpenAppend(p)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, data)
	w.Close()
	e, _ := f.Stat(p)
	return e
}

func get(t *testing.T, f cfs.FS, p string) string {
	t.Helper()
	r, err := f.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// source serves content[offset:] and records the offsets it was asked for; it
// delivers at most limit bytes (-1 for all) and then fails.
type source struct {
	offsets []int64
	data    string
	limit   int
}

func (s *source) open(offset int64) (io.ReadCloser, error) {
	s.offsets = append(s.offsets, offset)
	r := io.Reader(strings.NewReader(s.data[offset:]))
	if s.limit >= 0 {
		r = io.MultiReader(io.LimitReader(r, int64(s.limit)), errReader{})
	}
	return io.NopCloser(r), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func names(t *testing.T, f cfs.FS, dir string) string {
	t.Helper()
	es, _ := f.ReadDir(dir)
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}

func TestPlaceNewAndReplace(t *testing.T) {
	f := cfs.NewMem(cfs.MemOptions{})
	m := Meta{MtimeNs: 1_700_000_000_000_000_000, Exec: true}
	if err := Place(f, "a/b/new.sh", hash, (&source{data: content, limit: -1}).open, m, Expect{}); err != nil {
		t.Fatal(err)
	}
	e, _ := f.Stat("a/b/new.sh")
	if get(t, f, "a/b/new.sh") != content || e.MtimeNs != m.MtimeNs || !e.Exec {
		t.Errorf("new file: %+v", e)
	}
	if got := names(t, f, "a/b"); got != "new.sh" {
		t.Errorf("leftovers: %q", got)
	}

	old := put(t, f, "doc.txt", "old")
	err := Place(f, "doc.txt", hash, (&source{data: content, limit: -1}).open, m, Expect{true, old.Size, old.MtimeNs})
	if err != nil || get(t, f, "doc.txt") != content {
		t.Fatalf("replace: %v", err)
	}
	if got := names(t, f, ""); got != "a,doc.txt" {
		t.Errorf("leftovers: %q", got)
	}
}

func TestPlaceResumes(t *testing.T) {
	f := cfs.NewMem(cfs.MemOptions{})
	src := &source{data: content, limit: 10}
	if err := Place(f, "f", hash, src.open, Meta{}, Expect{}); err == nil {
		t.Fatal("expected failure")
	}
	if got := names(t, f, ""); got != ".syncpart-"+hash.String()[:16] {
		t.Fatalf("temp file: %q", got)
	}
	src.limit = -1
	if err := Place(f, "f", hash, src.open, Meta{}, Expect{}); err != nil {
		t.Fatal(err)
	}
	if want := []int64{0, 10}; src.offsets[0] != want[0] || src.offsets[1] != want[1] {
		t.Errorf("offsets = %v, want %v", src.offsets, want)
	}
	if get(t, f, "f") != content {
		t.Error("content wrong after resume")
	}
}

func TestPlaceTargetChanged(t *testing.T) {
	f := cfs.NewMem(cfs.MemOptions{})
	old := put(t, f, "f", "old")
	put(t, f, "f", "+edited") // modified after the caller looked
	err := Place(f, "f", hash, (&source{data: content, limit: -1}).open, Meta{}, Expect{true, old.Size, old.MtimeNs})
	if !errors.Is(err, ErrTargetChanged) || get(t, f, "f") != "old+edited" {
		t.Errorf("err=%v content=%q", err, get(t, f, "f"))
	}
	// Target appeared although none was expected, and target vanished although one was expected.
	if err := Place(f, "f", hash, (&source{data: content, limit: -1}).open, Meta{}, Expect{}); !errors.Is(err, ErrTargetChanged) {
		t.Errorf("unexpected target: %v", err)
	}
	if err := Place(f, "gone", hash, (&source{data: content, limit: -1}).open, Meta{}, Expect{Exists: true}); !errors.Is(err, ErrTargetChanged) {
		t.Errorf("vanished target: %v", err)
	}
}

func TestPlaceHashMismatch(t *testing.T) {
	f := cfs.NewMem(cfs.MemOptions{})
	old := put(t, f, "f", "old")
	bad := strings.Repeat("x", len(content))
	err := Place(f, "f", hash, (&source{data: bad, limit: -1}).open, Meta{}, Expect{true, old.Size, old.MtimeNs})
	if !errors.Is(err, ErrHashMismatch) || get(t, f, "f") != "old" {
		t.Fatalf("err=%v", err)
	}
	if got := names(t, f, ""); got != "f" {
		t.Errorf("temp file not deleted: %q", got)
	}
}

func TestPlaceLocked(t *testing.T) {
	f := cfs.NewMem(cfs.MemOptions{})
	old := put(t, f, "f", "old")
	f.Lock("f")
	src := &source{data: content, limit: -1}
	exp := Expect{true, old.Size, old.MtimeNs}
	if err := Place(f, "f", hash, src.open, Meta{}, exp); !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v", err)
	}
	if got := names(t, f, ""); got != ".syncpart-"+hash.String()[:16]+",f" {
		t.Errorf("temp file not kept: %q", got)
	}
	f.Unlock("f")
	if err := Place(f, "f", hash, src.open, Meta{}, exp); err != nil || get(t, f, "f") != content {
		t.Fatalf("after unlock: %v", err)
	}
	if src.offsets[len(src.offsets)-1] != int64(len(content)) {
		t.Errorf("second attempt should resume at the end: %v", src.offsets)
	}
}

func TestStageTwoFilesWithOneContentThenFinish(t *testing.T) {
	f := cfs.NewMem(cfs.MemOptions{})
	f.MkdirAll("d")
	put(t, f, "d/.syncpart-"+hash.String()[:16], "stale partial content") // an earlier attempt
	a, err := Stage(f, "d/a", hash, (&source{data: content, limit: -1}).open, Meta{}, Expect{}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Stage(f, "d/b", hash, (&source{data: content, limit: -1}).open, Meta{}, Expect{}, "-2", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Staged{b, a} {
		if err := s.Finish(); err != nil {
			t.Fatal(err)
		}
	}
	if get(t, f, "d/a") != content || get(t, f, "d/b") != content || names(t, f, "d") != "a,b" {
		t.Fatalf("after finish: %q %q %q", get(t, f, "d/a"), get(t, f, "d/b"), names(t, f, "d"))
	}
	if _, err := Stage(f, "d/c", hash, (&source{data: "other content", limit: -1}).open, Meta{}, Expect{}, "", true); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("wrong content: %v", err)
	}
	if names(t, f, "d") != "a,b" {
		t.Fatalf("temporary file left after a mismatch: %q", names(t, f, "d"))
	}
}

// Design 2.4: the space of a download is reserved before it starts; a disk that is too small fails the download
// at once, with nothing downloaded and nothing left behind that looks like a part of the file.
func TestPlaceFailsEarlyWithoutSpace(t *testing.T) {
	f := cfs.NewMem(cfs.MemOptions{Capacity: 10})
	src := &source{data: content, limit: -1}
	err := Place(f, "new", hash, src.open, Meta{Size: int64(len(content))}, Expect{})
	if !errors.Is(err, cfs.ErrNoSpace) {
		t.Fatalf("err = %v", err)
	}
	if len(src.offsets) != 0 {
		t.Fatal("content was requested although the disk is too small")
	}
	if _, err := f.Stat("new"); err == nil {
		t.Fatal("target created")
	}
	if err := Place(cfs.NewMem(cfs.MemOptions{Capacity: 100}), "new", hash, (&source{data: content, limit: -1}).open, Meta{Size: int64(len(content))}, Expect{}); err != nil {
		t.Fatal(err)
	}
}
