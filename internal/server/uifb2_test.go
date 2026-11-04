package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"kaze-sync/internal/proto"
)

// sized builds an add op of the given content with a modification time in unix seconds.
func (e *env) sized(path string, base int64, content string, mtime int64) proto.Op {
	op := e.op(path, base, content)
	op.MtimeNs = mtime * 1e9
	return op
}

// A directory row carries the total size and the latest modification time of the files below it, at any depth;
// deleted files are part of neither (design 4.5).
func TestDirectoryRowsSumSizeAndLatestMtime(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	e.commit(f, e.sized("d/a", 0, "123", 100), e.sized("d/sub/b", 0, "12345", 300), e.sized("d/sub/c", 0, "1234567", 200),
		e.sized("d/old", 0, "xxxxxxxxxxxx", 999), e.sized("z/x", 0, "zz", 50), e.sized("top", 0, "t", 10))
	var oldSeq, xSeq int64
	db, _ := e.s.folderDB(f)
	db.QueryRow(`SELECT seq FROM files WHERE path='d/old'`).Scan(&oldSeq)
	db.QueryRow(`SELECT seq FROM files WHERE path='z/x'`).Scan(&xSeq)
	e.commit(f, proto.Op{Path: "d/old", BaseSeq: oldSeq}, proto.Op{Path: "z/x", BaseSeq: xSeq})

	find := func(es []browseEntry, name string) browseEntry {
		for _, x := range es {
			if x.Name == name {
				return x
			}
		}
		t.Fatalf("no %q in %+v", name, es)
		return browseEntry{}
	}
	for _, deleted := range []bool{false, true} {
		es, err := e.s.browse(f, "", deleted)
		if err != nil {
			t.Fatal(err)
		}
		if d := find(es, "d"); d.Size != 15 || d.Time != 300 || d.Gone {
			t.Errorf("deleted=%v d: %+v, want size 15, time 300", deleted, d)
		}
		if find(es, "top").Size != 1 {
			t.Error("a file row changed")
		}
		if deleted {
			if z := find(es, "z"); !z.Gone || z.Size != 0 || z.Time != 0 {
				t.Errorf("z (only a deleted file): %+v", z)
			}
		}
	}
	es, _ := e.s.browse(f, "d", false)
	if sub := find(es, "sub"); sub.Size != 12 || sub.Time != 300 {
		t.Errorf("sub: %+v", sub)
	}
	// The page shows them, and none for a directory with only deleted content.
	_, page := e.get(c, "/folders/"+itoa(f)+"?deleted=1")
	row := page[strings.Index(page, `data-path="d"`):]
	row = row[:strings.Index(row, "</tr>")]
	if !strings.Contains(row, "15 B") || !strings.Contains(row, `datetime="1970-01-01T00:05:00Z"`) {
		t.Errorf("row d lacks the size or time:\n%s", row)
	}
	row = page[strings.Index(page, `data-path="z"`):]
	row = row[:strings.Index(row, "</tr>")]
	if strings.Contains(row, "<time") || strings.Contains(row, " B</td>") {
		t.Errorf("row z shows a size or time:\n%s", row)
	}
}

type ticksReply struct {
	Now, From int64
	Ticks     []int64
	None      bool
}

// The slider snaps to the end of every minute with a change in its scope, between the oldest state a restore can
// go back to and now.
func TestRestoreTicks(t *testing.T) {
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	f := e.folder("Docs")
	at := func(s string) time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Add(mustDur(t, s)) }
	step := func(to string) { e.clk.Add(at(to).Sub(e.clk.Now())) }
	seq := map[string]int64{}
	put := func(p, content string) {
		r := e.commit(f, e.op(p, seq[p], content))
		seq[p] = r[0].Seq
	}
	put("a", "a1") // the import: 12:00:00, :10, :20
	step("10s")
	put("dir/b", "b1")
	step("20s")
	put("c", "c1")
	step("30m25s")
	put("a", "a2") // 12:30:25: the end of its minute is 12:31:00
	step("31m10s")
	put("dir/b", "b2") // tick 12:32:00
	step("40m")
	e.commit(f, proto.Op{Path: "c", BaseSeq: seq["c"]}) // 12:40:00 exactly: tick 12:40:00
	step("45m30s")

	get := func(path string) ticksReply {
		code, body := e.get(c, "/folders/"+itoa(f)+"/restore-ticks?"+url.Values{"path": {path}}.Encode())
		var r ticksReply
		if code != 200 || json.Unmarshal([]byte(body), &r) != nil {
			t.Fatalf("%q: %d %s", path, code, body)
		}
		return r
	}
	unix := func(s ...string) []int64 {
		var out []int64
		for _, x := range s {
			out = append(out, at(x).Unix())
		}
		return out
	}
	same := func(got, want []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	r := get("")
	if want := unix("20s", "31m", "32m", "40m", "45m30s"); !same(r.Ticks, want) || r.From != want[0] || r.Now != want[4] || r.None {
		t.Errorf("folder: %+v, want %v", r, want)
	}
	// The import's own minutes are not ticks (nothing changed since its end); a directory has its own changes only.
	if r = get("dir"); !same(r.Ticks, unix("20s", "32m", "45m30s")) {
		t.Errorf("dir: %+v", r)
	}
	if r = get("nothing"); !r.None || len(r.Ticks) != 0 {
		t.Errorf("a path without history: %+v", r)
	}
	// The keep window bounds the slider: a day, then the changes inside it.
	SetMeta(e.s.db, "keep_days", 1)
	step("45m30s")
	e.clk.Add(47 * time.Hour)
	put("a", "a3")
	e.clk.Add(30 * time.Minute)
	now := e.clk.Now().Unix()
	r = get("")
	if r.From != now-86400 || len(r.Ticks) != 3 || r.Ticks[0] != r.From || r.Ticks[2] != now || r.Ticks[1] != (now-30*60+59)/60*60 {
		t.Errorf("window: %+v (now %d)", r, now)
	}
}

func mustDur(t *testing.T, s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A request the browser dropped (the slider moved on) stops the scan.
func TestRestorePreviewStopsWhenTheRequestIsCancelled(t *testing.T) {
	e := newEnv(t)
	f := e.folder("Docs")
	for _, n := range []string{"a", "b", "c"} {
		e.commit(f, e.op(n, 0, n+"1"))
		e.clk.Add(time.Minute)
	}
	e.commit(f, e.op("a", 1, "a2"))
	at := e.clk.Now().Add(-time.Hour)
	if n, _, _, _, err := e.s.restorePreview(context.Background(), f, "", at); err != nil || n == 0 {
		t.Fatalf("preview: %d %v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, _, err := e.s.restorePreview(ctx, f, "", at); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled preview: %v", err)
	}
}
