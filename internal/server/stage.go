package server

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/proto"
)

// Staged block lists of big files (design 2.4): the block list of a file with more than proto.MaxCommitBlocks blocks is
// sent in segments to var/stage/<device>-<folder>-<id>, 32 bytes per block, and the commit refers to it by id.
const (
	stageTTL = 24 * time.Hour // a staged list nobody appended to for this long is removed (GC) and no longer usable
	// stageMargin is kept free on the disk on top of the blocks a staged file still needs.
	stageMargin = 64 << 20
)

// freeSpace is freeBytes; tests replace it.
var freeSpace = freeBytes

var stageID = regexp.MustCompile(`^[0-9a-f]{32,64}$`)

func (s *Server) stageDir() string { return filepath.Join(s.varDir, "stage") }

func (s *Server) stagePath(dev, folder int64, id string) string {
	return filepath.Join(s.stageDir(), strconv.FormatInt(dev, 10)+"-"+strconv.FormatInt(folder, 10)+"-"+id)
}

func stageExpired(fi fs.FileInfo) bool { return time.Since(fi.ModTime()) > stageTTL }

// stageHave is the number of blocks received by a live staged file; ok is false when there is none.
func stageHave(path string) (n int, ok bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || stageExpired(fi) {
		return 0, false
	}
	return int(fi.Size() / proto.HashLen), true
}

// stageCount is how many live staged files the device has.
func (s *Server) stageCount(dev int64) int {
	ents, _ := os.ReadDir(s.stageDir())
	n := 0
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && strings.HasPrefix(e.Name(), strconv.FormatInt(dev, 10)+"-") && !stageExpired(fi) {
			n++
		}
	}
	return n
}

// stageHandler receives one segment of a staged block list.
func (s *Server) stageHandler(w http.ResponseWriter, r *http.Request, d device) {
	var req proto.StageReq
	if !readJSON(w, r, &req) {
		return
	}
	want := (req.Size + proto.BlockSize - 1) / proto.BlockSize
	switch {
	case !stageID.MatchString(req.ID), req.Size < 0, req.Offset < 0, len(req.Blocks) > proto.MaxStageBlocks,
		int64(req.Offset)+int64(len(req.Blocks)) > want:
		http.Error(w, "bad stage segment", http.StatusBadRequest)
		return
	}
	if !s.mapped(d.id, req.Folder) {
		http.Error(w, "folder not mapped to this device", http.StatusForbidden)
		return
	}
	resp, err := s.stage(d, req, want)
	if err != nil {
		slog.Error("stage", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) stage(d device, req proto.StageReq, want int64) (proto.StageResp, error) {
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	if want > proto.MaxFileBlocks {
		return proto.StageResp{Reason: proto.ReasonTooLarge}, nil
	}
	path := s.stagePath(d.id, req.Folder, req.ID)
	have, exists := stageHave(path)
	if !exists {
		if req.Offset > 0 {
			return proto.StageResp{Reason: proto.ReasonGap}, nil
		}
		if s.stageCount(d.id) >= proto.MaxStagesPerDevice {
			return proto.StageResp{Reason: proto.ReasonTooManyStages}, nil
		}
	}
	if req.Offset > have {
		return proto.StageResp{Have: have, Reason: proto.ReasonGap}, nil
	}
	var need []proto.Hash
	seen := map[proto.Hash]bool{}
	for _, h := range req.Blocks {
		if seen[h] {
			continue
		}
		seen[h] = true
		ok, err := s.blobs.Use(h)
		if err != nil {
			return proto.StageResp{}, err
		}
		if !ok {
			need = append(need, h)
		}
	}
	// Space: the missing blocks of this segment plus, for the rest of the file, the same share (the blocks are stored
	// once however many files use them, so this is an estimate, nothing is reserved).
	if free, ok := freeSpace(s.varDir); ok && len(req.Blocks) > 0 {
		rest := want - int64(req.Offset) - int64(len(req.Blocks))
		est := uint64(len(need))*proto.BlockSize + uint64(float64(rest)*float64(len(need))/float64(len(req.Blocks)))*proto.BlockSize
		if free < est+stageMargin {
			return proto.StageResp{Have: have, Reason: proto.ReasonNoSpace}, nil
		}
	}
	if err := os.MkdirAll(s.stageDir(), 0o755); err != nil {
		return proto.StageResp{}, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return proto.StageResp{}, err
	}
	buf := proto.JoinHashes(req.Blocks)
	if !exists {
		f.Truncate(0) // an expired file under the same name starts again
	}
	_, err = f.WriteAt(buf, int64(req.Offset)*proto.HashLen)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return proto.StageResp{}, err
	}
	return proto.StageResp{Have: max(have, req.Offset+len(req.Blocks)), Need: need}, nil
}

// loadStage returns the staged block list of op, which must hold exactly the blocks of op.Size.
func (s *Server) loadStage(devID, folder int64, op *proto.Op) bool {
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	want := (op.Size + proto.BlockSize - 1) / proto.BlockSize
	if !stageID.MatchString(op.Stage) || op.Size < 0 || want > proto.MaxFileBlocks {
		return false
	}
	path := s.stagePath(devID, folder, op.Stage)
	if have, ok := stageHave(path); !ok || int64(have) != want {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil || int64(len(b)) != want*proto.HashLen {
		return false
	}
	op.Blocks = proto.SplitHashes(b)
	return true
}

func (s *Server) dropStage(devID, folder int64, id string) {
	s.stageMu.Lock()
	os.Remove(s.stagePath(devID, folder, id))
	s.stageMu.Unlock()
}

// sweepStages removes the staged files nobody appended to for stageTTL.
func (s *Server) sweepStages() (removed int) {
	ents, _ := os.ReadDir(s.stageDir())
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && stageExpired(fi) && os.Remove(filepath.Join(s.stageDir(), e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// stageRefs is the refSource of the live staged files: their blocks are referenced although no commit names them yet,
// so a long upload does not lose its first blocks to the grace period. The files are read in pieces.
func (s *Server) stageRefs() refSource {
	paths := func() []string {
		ents, _ := os.ReadDir(s.stageDir())
		var out []string
		for _, e := range ents {
			if fi, err := e.Info(); err == nil && !stageExpired(fi) {
				out = append(out, filepath.Join(s.stageDir(), e.Name()))
			}
		}
		return out
	}
	return refSource{
		count: func() (int64, error) {
			var n int64
			for _, p := range paths() {
				if fi, err := os.Stat(p); err == nil {
					n += fi.Size() / proto.HashLen
				}
			}
			return n, nil
		},
		each: func(visit func([]byte) error) error {
			buf := make([]byte, 2048*proto.HashLen)
			for _, p := range paths() {
				f, err := os.Open(p)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				} else if err != nil {
					return err
				}
				for {
					n, err := io.ReadFull(f, buf)
					if n >= proto.HashLen {
						if verr := visit(buf[:n/proto.HashLen*proto.HashLen]); verr != nil {
							f.Close()
							return verr
						}
					}
					if err != nil {
						break
					}
				}
				f.Close()
			}
			return nil
		},
	}
}
