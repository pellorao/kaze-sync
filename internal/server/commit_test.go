package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

func status(rs []proto.Result) string {
	s := rs[0].Status
	if rs[0].Reason != "" {
		s += ":" + rs[0].Reason
	}
	return s
}

func (e *env) count(q string, args ...any) int { return e.countIn(e.s.db, q, args...) }

// fcount runs a query on a folder's database.
func (e *env) fcount(folder int64, q string, args ...any) int {
	db, err := e.s.folderDB(folder)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.countIn(db, q, args...)
}

func (e *env) countIn(db *sql.DB, q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// nextSeq is the next seq of a folder.
func (e *env) nextSeq(folder int64) int64 {
	db, err := e.s.folderDB(folder)
	if err != nil {
		e.t.Fatal(err)
	}
	return metaInt(db, "next_seq")
}

func TestCommitBaseVersionsAndTombstones(t *testing.T) {
	e := newEnv(t)
	f := e.folder("docs")

	r := e.commit(f, e.op("a.txt", 0, "one"))
	if status(r) != "ok" || r[0].Seq != 1 {
		t.Fatalf("add: %+v", r)
	}
	// Adding again with base 0 is a conflict that carries the current state.
	r = e.commit(f, e.op("a.txt", 0, "two"))
	if status(r) != "conflict" || r[0].Current == nil || r[0].Current.Seq != 1 {
		t.Fatalf("stale add: %+v", r)
	}
	if e.fcount(f, `SELECT count(*) FROM versions`) != 0 {
		t.Fatal("a refused op must not touch versions")
	}
	// Modify with the right base: old content moves to versions.
	r = e.commit(f, e.op("a.txt", 1, "two"))
	if status(r) != "ok" || r[0].Seq != 2 || e.fcount(f, `SELECT count(*) FROM versions WHERE path='a.txt'`) != 1 {
		t.Fatalf("modify: %+v", r)
	}
	// Delete with a stale base conflicts; with the right base it leaves a tombstone and a version.
	if r = e.commit(f, proto.Op{Path: "a.txt", BaseSeq: 1}); status(r) != "conflict" {
		t.Fatalf("stale delete: %+v", r)
	}
	r = e.commit(f, proto.Op{Path: "a.txt", BaseSeq: 2})
	if status(r) != "ok" || r[0].Seq != 3 || e.fcount(f, `SELECT count(*) FROM versions WHERE path='a.txt'`) != 2 {
		t.Fatalf("delete: %+v", r)
	}
	if e.fcount(f, `SELECT count(*) FROM files WHERE path='a.txt' AND hash IS NULL`) != 1 {
		t.Fatal("no tombstone")
	}
	// Deleting a tombstone, or a path that never existed, is a successful no-op.
	if r = e.commit(f, proto.Op{Path: "a.txt", BaseSeq: 0}); status(r) != "ok" || r[0].Seq != 3 {
		t.Fatalf("delete tombstone: %+v", r)
	}
	if r = e.commit(f, proto.Op{Path: "never", BaseSeq: 0}); status(r) != "ok" {
		t.Fatalf("delete missing: %+v", r)
	}
	if e.fcount(f, `SELECT count(*) FROM files WHERE path='never'`) != 0 || e.nextSeq(f) != 4 {
		t.Fatal("no-op delete changed state")
	}
	// A new file at a tombstoned path (base 0), and a modification based on the tombstone's seq, both work.
	if r = e.commit(f, e.op("a.txt", 0, "three")); status(r) != "ok" {
		t.Fatalf("add on tombstone: %+v", r)
	}
	e.commit(f, proto.Op{Path: "a.txt", BaseSeq: 4})
	if r = e.commit(f, e.op("a.txt", 5, "four")); status(r) != "ok" {
		t.Fatalf("modify over tombstone: %+v", r)
	}
	// Other folders are independent.
	g := e.folder("other")
	if r = e.commit(g, e.op("a.txt", 0, "x")); status(r) != "ok" {
		t.Fatalf("other folder: %+v", r)
	}
}

func TestCommitNeedsMissingBlocksOnly(t *testing.T) {
	e := newEnv(t)
	f := e.folder("docs")
	have := e.block([]byte("have"))
	miss := proto.Hash(sha256.Sum256([]byte("missing")))
	op := proto.Op{Path: "big", Hash: &miss, Blocks: []proto.Hash{have, miss, miss}, Size: 2*proto.BlockSize + 1}
	r := e.commit(f, op)
	if status(r) != "need" || len(r[0].Need) != 1 || r[0].Need[0] != miss {
		t.Fatalf("need: %+v", r)
	}
	if e.fcount(f, `SELECT count(*) FROM files`) != 0 || e.nextSeq(f) != 1 {
		t.Fatal("need must not change state")
	}
	e.block([]byte("missing"))
	// The stand-in blocks are not 1 MiB long but the server does not check lengths.
	if r = e.commit(f, op); status(r) != "ok" {
		t.Fatalf("after upload: %+v", r)
	}
}

func TestCommitNameRules(t *testing.T) {
	e := newEnv(t)
	f := e.folder("docs")
	for _, tc := range []struct{ path, want string }{
		{"A.txt", "ok"},
		{"a.txt", "rejected:case_collision"},
		{"A.txt", "conflict"}, // the same path again: base check first
		{"x", "ok"},
		{"x/y", "rejected:type_conflict"},
		{"d/y", "ok"},
		{"d", "rejected:type_conflict"},
		{"D/z", "ok"}, // directories may differ in case
		{"a:b.txt", "rejected:invalid_name"},
		{"dir/CON.txt", "rejected:invalid_name"},
		{"trailing.", "rejected:invalid_name"},
		{"../up", "rejected:invalid_name"},
		{"/abs", "rejected:invalid_name"},
	} {
		if got := status(e.commit(f, e.op(tc.path, 0, "c"))); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.path, got, tc.want)
		}
	}
	// Case-only rename: delete first, then add.
	seq := e.fcount(f, `SELECT seq FROM files WHERE path='A.txt'`)
	if r := e.commit(f, proto.Op{Path: "A.txt", BaseSeq: int64(seq)}); status(r) != "ok" {
		t.Fatalf("delete: %+v", r)
	}
	if r := e.commit(f, e.op("a.txt", 0, "c")); status(r) != "ok" {
		t.Fatalf("add after delete: %+v", r)
	}
}

func TestCommitFlagsConflictCopies(t *testing.T) {
	e := newEnv(t)
	f := e.folder("docs")
	e.commit(f, e.op("sub/report.conflict-laptop-20261101-153012.docx", 0, "c"), e.op("report.docx", 0, "c"))
	if n := e.fcount(f, `SELECT count(*) FROM files WHERE conflict AND hash IS NOT NULL`); n != 1 {
		t.Fatalf("conflict copies = %d", n)
	}
}

func TestCommitMalformed(t *testing.T) {
	e := newEnv(t)
	f := e.folder("docs")
	h := e.block([]byte("c"))
	for _, op := range []proto.Op{
		{Path: "a", Hash: &h, Blocks: nil, Size: 1},                // size needs one block
		{Path: "a", Hash: &h, Blocks: []proto.Hash{h, h}, Size: 1}, // too many blocks
		{Path: "a", Hash: &h, Blocks: []proto.Hash{h}, Size: -1},   // negative size
		{Path: "a", Blocks: []proto.Hash{h}},                       // delete with blocks
	} {
		if _, err := e.s.commit(f, "dev", []proto.Op{op}); err == nil {
			t.Errorf("%+v accepted", op)
		}
	}
}

func TestUploadBlocksKeepsCompleteOnes(t *testing.T) {
	e := newEnv(t)
	_, tok := e.enrollDevice("d1", "linux")
	var buf bytes.Buffer
	var hs []proto.Hash
	for _, c := range []string{"first", "second", "third"} {
		h := proto.Hash(sha256.Sum256([]byte(c)))
		hs = append(hs, h)
		proto.WriteBlock(&buf, h, []byte(c))
	}
	// Cut inside the third block's data.
	post := func(body []byte) int {
		req, _ := newBearerRequest(e.ts.URL+"/api/v1/blocks", tok, body)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(buf.Bytes()[:buf.Len()-2]); code != 400 {
		t.Fatalf("truncated upload: %d", code)
	}
	for i, want := range []bool{true, true, false} {
		if ok, _ := e.s.blobs.Has([32]byte(hs[i])); ok != want {
			t.Errorf("block %d stored = %v, want %v", i, ok, want)
		}
	}
	// A block whose content does not match its hash is refused.
	var bad bytes.Buffer
	proto.WriteBlock(&bad, hs[2], []byte("tampered"))
	if code := post(bad.Bytes()); code != 400 {
		t.Fatalf("bad hash: %d", code)
	}
	if code := post(buf.Bytes()); code != 200 {
		t.Fatalf("full upload: %d", code)
	}
}

// The name checks of a commit must use the key index: a scan per op makes a large commit quadratic.
func TestCommitNameCheckUsesIndex(t *testing.T) {
	e := newEnv(t)
	f := e.folder("F")
	db, err := e.s.folderDB(f)
	if err != nil {
		t.Fatal(err)
	}
	q, args := typeConflictQuery("a/b/c")
	rows, err := db.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(detail, "SCAN files") {
			t.Errorf("full scan in the type check: %s", detail)
		}
	}
}
