package server

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// excludedDevice is the device name of the deletions that saving exclusion rules makes (design 8.3). It cannot clash with
// a real device: their names are letters, digits, "_" and "-".
const excludedDevice = "web:rules"

// fileRow is a row of files or versions.
type fileRow struct {
	path, key string
	seq       int64
	hash      []byte // nil: tombstone
	blocks    []byte
	size      int64
	mtimeNs   int64
	exec      bool
	device    string
	createdAt int64
	conflict  bool
	link      bool
}

func (f *fileRow) scan(r interface{ Scan(...any) error }) error {
	return r.Scan(&f.path, &f.key, &f.seq, &f.hash, &f.blocks, &f.size, &f.mtimeNs, &f.exec, &f.device, &f.createdAt, &f.conflict, &f.link)
}

func (f *fileRow) entry() *proto.Entry {
	e := &proto.Entry{Path: f.path, Seq: f.seq, Size: f.size, MtimeNs: f.mtimeNs, Exec: f.exec, Link: f.link}
	if f.hash != nil {
		e.Hash = (*proto.Hash)(f.hash)
		e.Blocks = proto.SplitHashes(f.blocks)
	} else {
		e.Excluded = f.device == excludedDevice
	}
	return e
}

// badRequest marks errors caused by a malformed request.
type badRequest struct{ msg string }

func (e badRequest) Error() string { return e.msg }

func checkOp(op *proto.Op) error {
	if op.Hash == nil {
		if len(op.Blocks) != 0 {
			return badRequest{"delete with blocks"}
		}
		if op.Link {
			return badRequest{"delete of a link"}
		}
		return nil
	}
	want := (op.Size + proto.BlockSize - 1) / proto.BlockSize
	if op.Size < 0 || int64(len(op.Blocks)) != want {
		return badRequest{fmt.Sprintf("%s: %d blocks for size %d", op.Path, len(op.Blocks), op.Size)}
	}
	return nil
}

// commit applies ops to a folder in one transaction. Each op gets its own
// result; only ok results change state.
func (s *Server) commit(folder int64, device string, ops []proto.Op) ([]proto.Result, error) {
	return s.commitFor(folder, 0, device, ops)
}

// commitFor is commit for a device API request: devID (non-zero) lets an op name a staged block list (design 2.4).
func (s *Server) commitFor(folder, devID int64, device string, ops []proto.Op) ([]proto.Result, error) {
	var noStage map[int]bool
	for i := range ops {
		if ops[i].Stage != "" {
			if len(ops[i].Blocks) != 0 || ops[i].Hash == nil || devID == 0 {
				return nil, badRequest{"stage with blocks, or on a delete"}
			}
			if !s.loadStage(devID, folder, &ops[i]) {
				if noStage == nil {
					noStage = map[int]bool{}
				}
				noStage[i] = true
				continue
			}
		}
		if err := checkOp(&ops[i]); err != nil {
			return nil, err
		}
	}
	db, err := s.folderDB(folder)
	if err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	res := make([]proto.Result, len(ops))
	for i := range ops {
		if noStage[i] {
			res[i] = proto.Result{Status: proto.StatusRejected, Reason: proto.ReasonNoStage}
			continue
		}
		if res[i], err = s.commitOp(tx, device, &ops[i], now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for i, r := range res {
		if r.Status == proto.StatusOK && ops[i].Stage != "" {
			s.dropStage(devID, folder, ops[i].Stage)
		}
	}
	for _, r := range res {
		if r.Status == proto.StatusOK {
			s.broadcast() // wake the hanging polls of the devices that map this folder
			break
		}
	}
	return res, nil
}

func (s *Server) commitOp(tx *sql.Tx, device string, op *proto.Op, now int64) (proto.Result, error) {
	var cur fileRow
	have := true
	err := cur.scan(tx.QueryRow(`SELECT `+fileCols+` FROM files WHERE path=?`, op.Path))
	if errors.Is(err, sql.ErrNoRows) {
		have = false
	} else if err != nil {
		return proto.Result{}, err
	}
	conflict := func() proto.Result {
		r := proto.Result{Status: proto.StatusConflict}
		if have {
			r.Current = cur.entry()
		}
		return r
	}
	live := have && cur.hash != nil

	// Base check.
	if op.Hash == nil {
		if !live {
			return proto.Result{Status: proto.StatusOK, Seq: cur.seq}, nil // deleting a tombstone is a successful no-op
		}
		if op.BaseSeq != cur.seq {
			return conflict(), nil
		}
	} else if !(have && op.BaseSeq == cur.seq || !live && op.BaseSeq == 0) {
		return conflict(), nil
	}

	// Name checks.
	key := pp.Key(op.Path)
	if op.Hash != nil {
		if op.Link && (op.Size < 1 || op.Size > proto.MaxLinkTarget) {
			return proto.Result{Status: proto.StatusRejected, Reason: proto.ReasonInvalidLink}, nil
		}
		if reason, err := nameProblem(tx, op.Path, key); err != nil {
			return proto.Result{}, err
		} else if reason != "" {
			return proto.Result{Status: proto.StatusRejected, Reason: reason}, nil
		}
		var need []proto.Hash
		seen := map[proto.Hash]bool{}
		for _, b := range op.Blocks {
			if seen[b] {
				continue
			}
			seen[b] = true
			ok, err := s.blobs.Use(b)
			if err != nil {
				return proto.Result{}, err
			}
			if !ok {
				need = append(need, b)
			}
		}
		if need != nil {
			return proto.Result{Status: proto.StatusNeed, Need: need}, nil
		}
	}

	// Apply.
	if live {
		if _, err := tx.Exec(`INSERT INTO versions(`+fileCols+`, replaced_at)
			SELECT `+fileCols+`, ? FROM files WHERE path=?`, now, op.Path); err != nil {
			return proto.Result{}, err
		}
	}
	var seq int64
	if err := tx.QueryRow(`UPDATE meta SET value=value+1 WHERE key='next_seq' RETURNING value-1`).Scan(&seq); err != nil {
		return proto.Result{}, err
	}
	var hash, blocks []byte
	var size, mtime int64
	var exec, isConflict, link bool
	if op.Hash != nil {
		hash, blocks, size, mtime, exec, link = op.Hash[:], proto.JoinHashes(op.Blocks), op.Size, op.MtimeNs, op.Exec && !op.Link, op.Link
		isConflict = pp.IsConflictName(op.Path)
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO files(`+fileCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		op.Path, key, seq, hash, blocks, size, mtime, exec, device, now, isConflict, link)
	return proto.Result{Status: proto.StatusOK, Seq: seq}, err
}

// nameProblem returns the reason a live file at path cannot be added, or "".
func nameProblem(tx *sql.Tx, path, key string) (string, error) {
	if pp.CheckPortable(path) != nil {
		return proto.ReasonInvalidName, nil
	}
	var n int
	// Another live path with the same key.
	if err := tx.QueryRow(`SELECT count(*) FROM files WHERE key=? AND hash IS NOT NULL AND path<>?`, key, path).Scan(&n); err != nil || n > 0 {
		return proto.ReasonCaseCollision, err
	}
	q, args := typeConflictQuery(key)
	if err := tx.QueryRow(q, args...).Scan(&n); err != nil || n > 0 {
		return proto.ReasonTypeConflict, err
	}
	return "", nil
}

// typeConflictQuery counts the live files that clash in type with a live file at key: a file where one of its
// directories is needed, or files below it. The two conditions are separate selects: joined by OR, SQLite scans the
// whole index for every op, which made a large commit quadratic in the number of files.
func typeConflictQuery(key string) (string, []any) {
	q := `SELECT count(*) FROM (SELECT 1 FROM files WHERE hash IS NOT NULL AND key >= ? AND key < ?`
	args := []any{key + "/", key + "0"}
	var dirs []any
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			dirs = append(dirs, key[:i])
		}
	}
	if len(dirs) > 0 {
		q += ` UNION ALL SELECT 1 FROM files WHERE hash IS NOT NULL AND key IN (?` + strings.Repeat(`,?`, len(dirs)-1) + `)`
		args = append(args, dirs...)
	}
	return q + `)`, args
}
