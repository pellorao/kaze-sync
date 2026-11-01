package watch

import (
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// bufSize is the size of the change buffer in bytes; tests shrink it to force overflow.
var bufSize = 64 << 10

const filter = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_ATTRIBUTES | windows.FILE_NOTIFY_CHANGE_SIZE |
	windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_CREATION

type Watcher struct {
	dir, event, stop windows.Handle
	done             chan struct{}
	wg               sync.WaitGroup
	buf              []byte
	ov               windows.Overlapped
}

// Start watches the whole tree below root with one ReadDirectoryChangesW call
// at a time. After an unrecoverable error (such as the root being deleted) it
// sends "" and stops; the caller restarts it.
func Start(root string, out chan<- string) (*Watcher, error) {
	name, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	dir, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	w := &Watcher{dir: dir, done: make(chan struct{})}
	if w.event, err = windows.CreateEvent(nil, 1, 0, nil); err == nil {
		w.stop, err = windows.CreateEvent(nil, 1, 0, nil)
	}
	if err != nil {
		w.closeHandles()
		return nil, err
	}
	words := make([]uint32, bufSize/4) // DWORD-aligned
	w.buf = unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*4)
	w.ov.HEvent = w.event
	// Arm before returning: changes are only collected once a call is pending.
	if err := w.arm(); err != nil {
		w.closeHandles()
		return nil, err
	}
	w.wg.Add(1)
	go w.run(out)
	return w, nil
}

func (w *Watcher) arm() error {
	return windows.ReadDirectoryChanges(w.dir, &w.buf[0], uint32(len(w.buf)), true, filter, nil, &w.ov, 0)
}

func (w *Watcher) Add(string) error { return nil }

func (w *Watcher) Close() error {
	close(w.done)
	windows.SetEvent(w.stop)
	w.wg.Wait()
	w.closeHandles()
	return nil
}

func (w *Watcher) closeHandles() {
	for _, h := range []windows.Handle{w.dir, w.event, w.stop} {
		if h != 0 {
			windows.CloseHandle(h)
		}
	}
}

func (w *Watcher) run(out chan<- string) {
	defer w.wg.Done()
	send := func(dir string) {
		select {
		case out <- dir:
		case <-w.done:
		}
	}
	for {
		if r, _ := windows.WaitForMultipleObjects([]windows.Handle{w.event, w.stop}, false, windows.INFINITE); r != windows.WAIT_OBJECT_0 {
			windows.CancelIoEx(w.dir, &w.ov)
			var n uint32
			windows.GetOverlappedResult(w.dir, &w.ov, &n, true)
			return
		}
		var n uint32
		err := windows.GetOverlappedResult(w.dir, &w.ov, &n, false)
		switch {
		case err == windows.ERROR_NOTIFY_ENUM_DIR || (err == nil && n == 0):
			send("") // buffer overflowed: the changes were discarded
		case err != nil:
			send("")
			return
		}
		for off := uint32(0); n > 0; {
			rec := (*windows.FileNotifyInformation)(unsafe.Pointer(&w.buf[off]))
			name := windows.UTF16ToString(unsafe.Slice(&rec.FileName, rec.FileNameLength/2))
			send(filepath.ToSlash(filepath.Dir(name)))
			if rec.NextEntryOffset == 0 {
				break
			}
			off += rec.NextEntryOffset
		}
		if err := w.arm(); err != nil {
			send("")
			return
		}
	}
}
