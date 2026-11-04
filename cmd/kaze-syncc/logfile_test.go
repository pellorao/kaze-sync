package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogFileRotatesWhileRunning(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kaze-syncc.log")
	l, err := openLog(p, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 5000; i++ { // 500 KB of warnings
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{p, p + ".old"} {
		fi, err := os.Stat(f)
		if err != nil || fi.Size() > 1000 {
			t.Fatalf("%s: %v %v", f, fi, err)
		}
	}
}
