// Package watch reports which directories below a root had changes.
//
// Start watches root and sends the changed directory (slash-separated,
// relative to root, "." for the root itself) to out. An empty string means
// events may have been lost: the caller must rescan everything. A blocked out
// delays reading further events.
//
//	func Start(root string, out chan<- string) (*Watcher, error)
//	func (w *Watcher) Add(dir string) error
//	func (w *Watcher) Close() error
//
// Add makes sure dir (relative to root) is watched. On Linux watches are not
// recursive, so the scanner calls Add for every directory it visits and Add
// fails when the inotify watch limit is exhausted. On Windows the whole tree
// is watched from the start and Add does nothing.
package watch
