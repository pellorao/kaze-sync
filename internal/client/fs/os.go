package fs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
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
		IsLink:    m&os.ModeSymlink != 0,
		Size:      fi.Size(),
		MtimeNs:   fi.ModTime().UnixNano(),
		Exec:      runtime.GOOS != "windows" && m&0o100 != 0,
	}
}

func (o osFS) ReadDir(dir string) ([]Entry, error) {
	if err := o.guard(dir + "/"); err != nil {
		return nil, err
	}
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
	if err := o.guard(p); err != nil {
		return Entry{}, err
	}
	fi, err := os.Lstat(o.abs(p))
	if err != nil {
		return Entry{}, err
	}
	return entry(fi), nil
}

// guard fails with ErrLinkOnPath when a directory above p, below the root, is a symbolic link. Every operation that
// touches the file system calls it first: the sync engine never works below a link, so one found there was planted
// or replaced meanwhile, and following it could write outside the root.
func (o osFS) guard(p string) error {
	for i := 0; i < len(p); i++ {
		if p[i] != '/' || i == 0 {
			continue
		}
		fi, err := os.Lstat(o.abs(p[:i]))
		if err != nil {
			return nil // missing: the operation itself reports it
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: %w", p[:i], ErrLinkOnPath)
		}
	}
	return nil
}

func (o osFS) Open(p string) (io.ReadCloser, error) {
	if err := o.guard(p); err != nil {
		return nil, err
	}
	f, err := os.Open(o.abs(p))
	return f, wrapLocked(err)
}

func (o osFS) OpenAppend(p string) (io.WriteCloser, int64, error) {
	if err := o.guard(p); err != nil {
		return nil, 0, err
	}
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
	if err := o.guard(p); err != nil {
		return err
	}
	f, err := os.OpenFile(o.abs(p), os.O_RDWR, 0)
	if err != nil {
		return wrapLocked(err)
	}
	defer f.Close()
	return f.Sync()
}

func (o osFS) Rename(oldp, newp string) error {
	if err := errors.Join(o.guard(oldp), o.guard(newp)); err != nil {
		return err
	}
	return wrapRename(os.Rename(o.abs(oldp), o.abs(newp)))
}

func (o osFS) Remove(p string) error {
	if err := o.guard(p); err != nil {
		return err
	}
	return wrapLocked(os.Remove(o.abs(p)))
}

func (o osFS) MkdirAll(p string) error {
	if err := o.guard(p + "/"); err != nil { // the directory itself too
		return err
	}
	return os.MkdirAll(o.abs(p), 0o755)
}

func (o osFS) Readlink(p string) (string, error) {
	if err := o.guard(p); err != nil {
		return "", err
	}
	return os.Readlink(o.abs(p))
}

func (o osFS) Symlink(target, p string) error {
	if err := o.guard(p); err != nil {
		return err
	}
	err := os.Symlink(target, o.abs(p))
	var en syscall.Errno
	if runtime.GOOS == "windows" && errors.As(err, &en) && en == 1314 { // ERROR_PRIVILEGE_NOT_HELD
		return fmt.Errorf("%w: %v", ErrLinkUnsupported, err)
	}
	return err
}

// SetMeta sets the mtime and, except on Windows, adds or clears the execute bits.
func (o osFS) SetMeta(p string, mtimeNs int64, exec bool) error {
	if err := o.guard(p); err != nil {
		return err
	}
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
