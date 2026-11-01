package fs

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type osFS struct{ root string }

// OS returns the real file system below the directory root.
func OS(root string) FS { return osFS{root} }

func (o osFS) abs(p string) string { return filepath.Join(o.root, filepath.FromSlash(p)) }

func entry(fi os.FileInfo) Entry {
	m := fi.Mode()
	return Entry{
		Name:      fi.Name(),
		IsDir:     m.IsDir(),
		IsSymlink: m&(os.ModeSymlink|os.ModeIrregular) != 0,
		Size:      fi.Size(),
		MtimeNs:   fi.ModTime().UnixNano(),
		Exec:      runtime.GOOS != "windows" && m&0o100 != 0,
	}
}

func (o osFS) ReadDir(dir string) ([]Entry, error) {
	des, err := os.ReadDir(o.abs(dir))
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		fi, err := de.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, entry(fi))
	}
	return out, nil
}

func (o osFS) Stat(p string) (Entry, error) {
	fi, err := os.Lstat(o.abs(p))
	if err != nil {
		return Entry{}, err
	}
	return entry(fi), nil
}

func (o osFS) Open(p string) (io.ReadCloser, error) {
	f, err := os.Open(o.abs(p))
	return f, wrapLocked(err)
}

func (o osFS) OpenAppend(p string) (io.WriteCloser, int64, error) {
	f, err := os.OpenFile(o.abs(p), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, 0, wrapLocked(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

func (o osFS) Sync(p string) error {
	f, err := os.OpenFile(o.abs(p), os.O_RDWR, 0)
	if err != nil {
		return wrapLocked(err)
	}
	defer f.Close()
	return f.Sync()
}

func (o osFS) Rename(oldp, newp string) error {
	return wrapRename(os.Rename(o.abs(oldp), o.abs(newp)))
}

func (o osFS) Remove(p string) error { return wrapLocked(os.Remove(o.abs(p))) }

func (o osFS) MkdirAll(p string) error { return os.MkdirAll(o.abs(p), 0o755) }

// SetMeta sets the mtime and, except on Windows, adds or clears the execute bits.
func (o osFS) SetMeta(p string, mtimeNs int64, exec bool) error {
	abs := o.abs(p)
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(abs)
		if err != nil {
			return err
		}
		mode := fi.Mode().Perm()
		if exec {
			mode |= (mode & 0o444) >> 2
		} else {
			mode &^= 0o111
		}
		if err := os.Chmod(abs, mode); err != nil {
			return err
		}
	}
	t := time.Unix(0, mtimeNs)
	return os.Chtimes(abs, t, t)
}
