package server

import (
	"regexp"
	"sort"
	"strings"
)

// maxFilter is the longest glob filter the previews take.
const maxFilter = 200

// previewMax is the number of paths a preview lists (the restore preview and the rule preview): the person filters
// to see the rest.
const previewMax = 50

// globFilter compiles the one-line glob the previews filter their lists with (design 4.6): a pattern without "/"
// matches the file name, one with "/" the whole path; "*" stays inside a name, "**" crosses "/", "?" is one
// character; case is ignored. An empty pattern keeps everything (nil). The browser never matches: it asks the server,
// which holds the full list.
func globFilter(g string) func(path string) bool {
	g = strings.TrimSpace(g)
	if g == "" || len(g) > maxFilter {
		return nil
	}
	var b strings.Builder
	b.WriteString("(?is)^")
	rs := []rune(g)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case '*':
			if i+1 < len(rs) && rs[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	if !strings.Contains(g, "/") {
		return func(p string) bool { return re.MatchString(p[strings.LastIndexByte(p, '/')+1:]) }
	}
	return re.MatchString
}

// firstPaths keeps the previewMax smallest items of a stream by path, in order, whatever the order they arrive in.
type firstPaths[T any] struct {
	items []T
	path  func(T) string
}

func (f *firstPaths[T]) add(it T) {
	p := f.path(it)
	i := sort.Search(len(f.items), func(i int) bool { return f.path(f.items[i]) > p })
	if i >= previewMax {
		return
	}
	f.items = append(f.items, it)
	copy(f.items[i+1:], f.items[i:])
	f.items[i] = it
	if len(f.items) > previewMax {
		f.items = f.items[:previewMax]
	}
}
