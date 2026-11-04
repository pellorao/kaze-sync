package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"sync"

	cfs "kaze-sync/internal/client/fs"
	"kaze-sync/internal/client/place"
	"kaze-sync/internal/proto"
	pp "kaze-sync/internal/proto/path"
)

// Limits of one batch of downloads: its files share one /blocks/get request and one response. getBatchBytes bounds
// the response (a larger file is fetched alone, in windows) and with it the memory a batch holds, maxBatchFiles
// the temporary files on disk at once. ioWorkers is how many files are written at the same time.
var (
	getBatchBytes int64 = 16 << 20
	ioWorkers           = 16
)

const maxBatchFiles = 1000

// fetchJob is a file to download with what Pull decided about it; err is the outcome.
type fetchJob struct {
	e      proto.Entry
	src    *blockSrc
	expect place.Expect
	err    error

	row localRow // what to store once the file is in place
}

// fetchQueue collects the files of the next batch.
type fetchQueue struct {
	jobs          []*fetchJob
	bytes, hashes int64
}

// fits reports whether e can join the queue: the upper bound of the response (every block remote) stays within the limits.
func (q *fetchQueue) fits(e proto.Entry) bool {
	return len(q.jobs) < maxBatchFiles && q.bytes+e.Size <= getBatchBytes && q.hashes+int64(len(e.Blocks)) <= proto.MaxGetBlocks
}

func (q *fetchQueue) add(j *fetchJob) {
	q.jobs = append(q.jobs, j)
	q.bytes += j.e.Size
	q.hashes += int64(len(j.e.Blocks))
}

// fetchBatch downloads the files of a queue (design 5.5): one request for the blocks no local file has, read
// from the response in order (the queue's size limit bounds what is in memory), while the files are written by
// several workers at once, each with its own fsync, then one transaction for the base rows. Writing a small
// file is mostly waiting (fsync, and on Windows the virus scanner), so the files overlap. A file that fails for a
// reason of its own is tried again alone, with the full handling of fetch. A failure of the network ends the
// batch: the files whose blocks arrived before it are finished, the others have err set to a remoteError.
func (f *Folder) fetchBatch(ctx context.Context, jobs []*fetchJob) []*fetchJob {
	if len(jobs) == 0 {
		return nil
	}
	readers := make([]*blockReader, len(jobs))
	wants := make([][]proto.Hash, len(jobs))
	var want []proto.Hash
	for i, j := range jobs {
		j.src = f.localSource(&j.e, j.src)
		readers[i] = newBlockReader(ctx, f, &j.e, j.src, 0)
		wants[i] = readers[i].plan(0, len(j.e.Blocks))
		want = append(want, wants[i]...)
	}
	var body io.ReadCloser
	var rerr error
	if len(want) > 0 {
		if body, rerr = f.API.GetBlocks(ctx, want); rerr == nil {
			defer body.Close()
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var panicked any
	sem := make(chan struct{}, ioWorkers)
	used := map[string]bool{}
	pbuf := make([]byte, proto.BlockSize)
	for i, j := range jobs {
		if rerr == nil {
			readers[i].prefetch = make(map[proto.Hash][]byte, len(wants[i]))
			for _, h := range wants[i] {
				got, data, err := proto.ReadBlock(body, pbuf)
				if err == nil && got != h {
					err = fmt.Errorf("server sent block %s, want %s", got, h)
				}
				if err != nil {
					rerr = err
					break
				}
				readers[i].prefetch[h] = append([]byte(nil), data...)
			}
		}
		if rerr != nil {
			j.err = remoteError{rerr}
			readers[i].prefetch = nil
			continue
		}
		// Two files of one directory with the same content would share a temporary name.
		suffix, tmp := "", pp.Key(path.Dir(j.e.Path))+"/"+j.e.Hash.String()[:16]
		for n := 2; used[tmp+suffix]; n++ {
			suffix = "-" + strconv.Itoa(n)
		}
		used[tmp+suffix] = true
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() {
				if p := recover(); p != nil { // raised again below, in the caller's goroutine
					mu.Lock()
					panicked = p
					mu.Unlock()
				}
				readers[i].prefetch = nil
				<-sem
				wg.Done()
			}()
			j.err = f.placeJob(j, readers[i], suffix)
		}()
	}
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
	var rows []localRow
	for _, j := range jobs {
		if j.err == nil {
			rows = append(rows, j.row)
		}
	}
	if err := f.DB.apply(rows, nil); err != nil {
		for _, j := range jobs {
			if j.err == nil {
				j.err = err
			}
		}
	}
	// What failed for a reason of its own: the single way.
	for _, j := range jobs {
		switch {
		case j.err == nil, isRemote(j.err), ctx.Err() != nil:
		case errors.Is(j.err, place.ErrTargetChanged), errors.Is(j.err, cfs.ErrLocked):
		default:
			j.err = f.fetch(ctx, &j.e, j.src, j.expect)
		}
	}
	return jobs
}

// placeJob writes one file of a batch from the reader and records the row to store for it.
func (f *Folder) placeJob(j *fetchJob, r *blockReader, suffix string) error {
	open := func(int64) (io.ReadCloser, error) { return r, nil }
	s, err := place.Stage(f.FS, j.e.Path, *j.e.Hash, open, place.Meta{MtimeNs: j.e.MtimeNs, Exec: j.e.Exec}, j.expect, suffix, true)
	if err != nil {
		return err
	}
	if err := s.Finish(); err != nil {
		return err
	}
	j.row, err = f.placedRow(&j.e)
	return err
}
