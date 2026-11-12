package server

import (
	"regexp"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

// The head line of the folder page counts the current directory and below (design 4.5): files and size from the
// listing's own scan (deleted files left out), errors and reminders from the notices by path; the device names are
// in a tip, and a held tag is a link per device.
func TestFolderHeadCountsCurrentDirectory(t *testing.T) {
	l := newLockEnv(t)
	e := l.env
	e.commit(l.f, e.sized("d/a", 0, "123", 100), e.sized("d/sub/b", 0, "12345", 300), e.sized("d/sub/c", 0, "1234567", 200),
		e.sized("d/old", 0, "xxxxxxxxxxxx", 999), e.sized("top", 0, "t", 10))
	db, _ := e.s.folderDB(l.f)
	var oldSeq int64
	db.QueryRow(`SELECT seq FROM files WHERE path='d/old'`).Scan(&oldSeq)
	e.commit(l.f, proto.Op{Path: "d/old", BaseSeq: oldSeq})
	e.report(l.ta, l.f,
		proto.FileError{Path: "d/sub/b", Kind: proto.KindIO, Message: "boom", Since: 1},
		proto.FileError{Path: "top", Kind: proto.KindInUse, Message: "busy", Since: 1})
	e.heldReport(l.ta, l.f, proto.HeldItem{Path: "d/sub/h1", Scope: "d:d/sub", Reason: "owner", Kind: "modify", At: 1},
		proto.HeldItem{Path: "d/sub/h2", Scope: "d:d/sub", Reason: "owner", Kind: "modify", At: 1})
	e.heldReport(l.tb, l.f, proto.HeldItem{Path: "d/sub/h3", Scope: "d:d/sub", Reason: "owner", Kind: "modify", At: 1})

	head := func(q string) string {
		_, page := e.get(l.web, "/folders/"+itoa(l.f)+q)
		h := page[strings.Index(page, `<p class="sub">`):]
		return h[:strings.Index(h, "</p>")]
	}
	vis := func(h string) string { // the text a person sees: tips are hidden
		h = regexp.MustCompile(`<span class="tip-c"[^>]*>.*?</span></span>`).ReplaceAllString(h, "")
		return regexp.MustCompile(`<[^>]+>`).ReplaceAllString(h, " ")
	}
	top := head("")
	if v := vis(top); !strings.Contains(v, "4 个文件") {
		t.Errorf("top head: %s", v)
	}
	d := head("?dir=d")
	v := vis(d)
	for _, want := range []string{"3 个文件", "15 B", "2 台设备", "3 个错误"} {
		if !strings.Contains(v, want) {
			t.Errorf("dir d head lacks %q: %s", want, v)
		}
	}
	if strings.Contains(v, "A,") || strings.Contains(v, "1 个提醒") { // names only in the tip; "top"'s reminder is outside d
		t.Errorf("dir d head: %s", v)
	}
	if !strings.Contains(d, `<span class="tp">A, B</span>`) {
		t.Errorf("device tip: %s", d)
	}
	// the held notice (scope d/sub) is an error of d and of d/sub, not of the sibling or the root-only counts
	if !strings.Contains(vis(head("?dir=d/sub")), "3 个错误") {
		t.Errorf("d/sub: %s", vis(head("?dir=d/sub")))
	}
	if s := vis(head("?dir=d/none")); strings.Contains(s, "个错误") || strings.Contains(s, "个提醒") {
		t.Errorf("empty dir shows notices: %s", s)
	}
	// the row of d in the top listing: one link tag per device, each opening that device's dialog
	_, page := e.get(l.web, "/folders/"+itoa(l.f))
	row := page[strings.Index(page, `data-path="d"`):]
	row = row[:strings.Index(row, "</tr>")]
	for _, want := range []string{`href="/folders/1?dir=&amp;held=` + itoa(l.a) + `"`, `href="/folders/1?dir=&amp;held=` + itoa(l.b) + `"`} {
		if !strings.Contains(row, want) {
			t.Errorf("row d lacks %s:\n%s", want, row)
		}
	}
	if n := strings.Count(row, `class="tag err lnk"`); n != 2 || !strings.Contains(row, ">待处理 2 · A") || !strings.Contains(row, ">待处理 1 · B") {
		t.Errorf("held tags (%d):\n%s", n, row)
	}
}

// The sync status word (syncing, error, offline ...) describes the whole folder, so only the top level shows it; a
// subdirectory head, which counts its own directory, has none and no stray separator (design 4.5, #437).
func TestFolderHeadStateOnlyAtTop(t *testing.T) {
	l := newLockEnv(t)
	e := l.env
	e.commit(l.f, e.sized("d/a", 0, "123", 100), e.sized("top", 0, "t", 10))
	e.report(l.ta, l.f, proto.FileError{Path: "top", Kind: proto.KindInUse, Message: "busy", Since: 1})
	head := func(q string) string {
		_, page := e.get(l.web, "/folders/"+itoa(l.f)+q)
		h := page[strings.Index(page, `<p class="sub">`):]
		return h[:strings.Index(h, "</p>")]
	}
	stateSpan := regexp.MustCompile(`<span class="st-[a-z]+"`)
	if top := head(""); !stateSpan.MatchString(top) {
		t.Errorf("top head lacks the state word: %s", top)
	}
	for _, q := range []string{"?dir=d", "?dir=d&deleted=1", "?dir=d&file=d/a"} {
		d := head(q)
		if stateSpan.MatchString(d) {
			t.Errorf("%s head shows the state word: %s", q, d)
		}
		if strings.Contains(d, `<span class="dot-sep"></span>`) {
			t.Errorf("%s head has an empty separator: %s", q, d)
		}
		if !strings.Contains(d, "1 个文件") {
			t.Errorf("%s head lacks the file count: %s", q, d)
		}
	}
}

func TestNoticeIn(t *testing.T) {
	cases := []struct {
		n    notice
		dir  string
		want bool
	}{
		{notice{Path: "a/b"}, "", true},
		{notice{Path: "a/b"}, "a", true},
		{notice{Path: "a/b"}, "a/b", true},
		{notice{Path: "a/bc"}, "a/b", false},
		{notice{Path: "x"}, "a", false},
		{notice{}, "a", false},
		{notice{Kind: kindHeld, Items: []string{"a/b/x"}}, "a", true},
		{notice{Kind: kindHeld, Items: []string{"a/x"}}, "a/b", false},
	}
	for _, c := range cases {
		if got := noticeIn(c.n, c.dir); got != c.want {
			t.Errorf("noticeIn(%+v, %q) = %v", c.n, c.dir, got)
		}
	}
}
