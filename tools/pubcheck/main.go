// Command pubcheck scans the files of the repository for text that must not be published: personal requirement
// notes ("用户 <date>"), paths on the maintainer's machine, the names of private projects and internal task names.
// It prints every hit and exits with status 1 if there is any, so it can run before a release:
//
//	go run ./tools/pubcheck [-root DIR]
//
// The files are those Git would publish (tracked plus untracked, not ignored); without Git it walks the tree and
// skips .git and .tmp. Binary files are skipped.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A rule is one kind of text that must not be published. ok, if set, rejects a match of re that is not really a
// hit (Go regexps have no lookaround).
type rule struct {
	name string
	re   *regexp.Regexp
	ok   func(line string, loc []int) bool
}

// The patterns are written so that this file does not match them itself (e.g. "project[s]").
var rules = []rule{
	{name: "user note", re: regexp.MustCompile(`用户 ?20[0-9]{2}-`)},
	{name: "local path", re: regexp.MustCompile(`(~|\$HOME|\$\{HOME\}|/home/[a-z_][a-z0-9_-]*)/project[s]\b`)},
	{name: "private project", re: regexp.MustCompile(`(?i)\bstrat[a]\b|qwen-strat[a]|kaze-worksho[p]`)},
	{name: "private project", re: regexp.MustCompile(`\bAR[W]\b|release-v[m]\.mjs|restore-chec[k]\.py`)},
	{name: "internal task name", re: regexp.MustCompile(`(^|[^/.\w-])(kaze-sync/[a-z][a-z0-9-]*)`), ok: taskNameHit},
}

// taskNameHit tells an internal task name (kaze-sync/<task>) from a path such as "/var/lib/kaze-sync/var" or
// "~/.kaze-sync/server": the name is not preceded by '/', '.', a word character or '-' (checked by the pattern)
// and not followed by '/', '.', a word character or '-'.
func taskNameHit(line string, loc []int) bool {
	end := loc[5]
	if end < len(line) {
		c := line[end]
		if c == '/' || c == '.' || c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			return false
		}
	}
	return true
}

// A hit is one match in one line.
type hit struct {
	path string
	line int
	rule string
	text string
}

func (h hit) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", h.path, h.line, h.rule, h.text)
}

// scanText returns the hits in the text of one file.
func scanText(path string, data []byte) []hit {
	var hits []hit
	for i, line := range strings.Split(string(data), "\n") {
		for _, r := range rules {
			for _, loc := range r.re.FindAllStringSubmatchIndex(line, -1) {
				if r.ok != nil && !r.ok(line, loc) {
					continue
				}
				hits = append(hits, hit{path: path, line: i + 1, rule: r.name, text: excerpt(line, loc[0], loc[1])})
			}
		}
	}
	return hits
}

// excerpt returns the match with up to 40 bytes of context on each side, cut at rune boundaries.
func excerpt(line string, a, b int) string {
	lo, hi := a-40, b+40
	if lo < 0 {
		lo = 0
	}
	if hi > len(line) {
		hi = len(line)
	}
	for lo > 0 && lo < len(line) && line[lo]&0xC0 == 0x80 {
		lo--
	}
	for hi < len(line) && line[hi]&0xC0 == 0x80 {
		hi++
	}
	return strings.TrimSpace(line[lo:hi])
}

// isBinary reports whether data looks like a binary file (a NUL byte in the first 8000 bytes, as Git does).
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

// listFiles returns the files under root that would be published, relative to root and with '/' separators.
func listFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Stderr = io.Discard
	if out, err := cmd.Output(); err == nil {
		var files []string
		for _, f := range strings.Split(string(out), "\x00") {
			if f != "" {
				files = append(files, f)
			}
		}
		sort.Strings(files)
		return dedup(files), nil
	}
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".tmp") && p != root {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func dedup(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// scan returns the hits in all published files under root.
func scan(root string) ([]hit, error) {
	files, err := listFiles(root)
	if err != nil {
		return nil, err
	}
	var hits []hit
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if errors.Is(err, fs.ErrNotExist) {
			continue // tracked but deleted in the working tree
		}
		if err != nil {
			return nil, err
		}
		if isBinary(data) {
			continue
		}
		hits = append(hits, scanText(f, data)...)
	}
	return hits, nil
}

func main() {
	root := flag.String("root", ".", "repository root to scan")
	flag.Parse()
	hits, err := scan(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pubcheck:", err)
		os.Exit(2)
	}
	for _, h := range hits {
		fmt.Println(h)
	}
	if len(hits) > 0 {
		fmt.Fprintf(os.Stderr, "pubcheck: %d hit(s)\n", len(hits))
		os.Exit(1)
	}
	fmt.Println("pubcheck: clean")
}
