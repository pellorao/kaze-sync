// Package ignore matches paths against gitignore-style rules.
package ignore

import "strings"

type LineError struct {
	Line int // 1-based
	Msg  string
}

type rule struct {
	pat      string
	negate   bool
	dirOnly  bool
	anchored bool // pattern contains a slash: match the whole path, not the base name
}

type Matcher struct {
	rules []rule
	fold  bool
}

// Compile compiles rule lines; an invalid line yields a LineError and takes no part in matching.
func Compile(lines []string, caseFold bool) (*Matcher, []LineError) {
	m := &Matcher{fold: caseFold}
	var errs []LineError
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || line[0] == '#' {
			continue
		}
		line = trimTrailingSpaces(line)
		var r rule
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = line[:len(line)-1]
		}
		if line == "" {
			continue
		}
		if msg := malformed(line); msg != "" {
			errs = append(errs, LineError{i + 1, msg})
			continue
		}
		if r.anchored = strings.Contains(line, "/"); r.anchored {
			line = strings.TrimPrefix(line, "/")
		}
		r.pat = line
		m.rules = append(m.rules, r)
	}
	return m, errs
}

// trimTrailingSpaces removes trailing spaces that are not escaped by a backslash.
func trimTrailingSpaces(s string) string {
	lastSpace := -1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ':
			if lastSpace < 0 {
				lastSpace = i
			}
		case '\\':
			if i++; i == len(s) {
				return s
			}
			lastSpace = -1
		default:
			lastSpace = -1
		}
	}
	if lastSpace >= 0 {
		return s[:lastSpace]
	}
	return s
}

// Excluded reports whether one entry is excluded, assuming all its ancestor directories are not.
func (m *Matcher) Excluded(path string, isDir bool) bool {
	base := path[strings.LastIndexByte(path, '/')+1:]
	for i := len(m.rules) - 1; i >= 0; i-- {
		r := &m.rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		name := base
		if r.anchored {
			name = path
		}
		if wildmatch(r.pat, name, m.fold) {
			return !r.negate
		}
	}
	return false
}

// ExcludedPath reports whether any path is excluded, checking every ancestor directory first.
func (m *Matcher) ExcludedPath(path string, isDir bool) bool {
	for i := 0; i < len(path); i++ {
		if path[i] == '/' && m.Excluded(path[:i], true) {
			return true
		}
	}
	return m.Excluded(path, isDir)
}
