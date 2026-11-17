//go:build !windows

package watch

import (
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

type Watcher struct {
	root string
	w    *fsnotify.Watcher
	done chan struct{}
}

func Start(root string, out chan<- string) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{root: root, w: fw, done: make(chan struct{})}
	if err := w.Add("."); err != nil {
		fw.Close()
		return nil, err
	}
	send := func(dir string) {
		select {
		case out <- dir:
		case <-w.done:
		}
	}
	go func() {
		for {
			select {
			case ev, ok := <-fw.Events:
				if !ok {
					return
				}
				rel, _ := filepath.Rel(root, filepath.Dir(ev.Name))
				rel = filepath.ToSlash(rel)
				if rel == ".." || strings.HasPrefix(rel, "../") {
					rel = "." // an attribute event on the root itself: its parent directory is outside the folder
				}
				send(rel)
			case _, ok := <-fw.Errors:
				if !ok {
					return
				}
				send("") // overflow or read error: events may be lost
			}
		}
	}()
	return w, nil
}

func (w *Watcher) Add(dir string) error {
	return w.w.Add(filepath.Join(w.root, filepath.FromSlash(dir)))
}

func (w *Watcher) Close() error {
	close(w.done)
	return w.w.Close()
}
