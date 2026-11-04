package main

import (
	"os"
	"sync"
)

// logFile is the agent's log: an append-only file that moves to <path>.old once it would grow past max bytes,
// also while the agent runs (a loop that logs a warning each round must not fill the disk). Only one .old is kept.
type logFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func openLog(path string, max int64) (*logFile, error) {
	l := &logFile{path: path, max: max}
	if fi, err := os.Stat(path); err == nil && fi.Size() > max {
		os.Rename(path, path+".old")
	}
	return l, l.open(0)
}

func (l *logFile) open(flag int) error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|flag, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *logFile) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(b)) > l.max {
		l.f.Close()
		flag := 0
		if os.Rename(l.path, l.path+".old") != nil {
			flag = os.O_TRUNC // another program holds the file (Windows): start it over instead
		}
		if err := l.open(flag); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	return n, err
}

func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
