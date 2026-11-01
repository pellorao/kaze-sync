package ignore

import (
	"fmt"
	"testing"
	"time"
)

func TestWildmatchVectors(t *testing.T) {
	for _, v := range wildmatchVectors {
		if got := wildmatch(v.pat, v.text, false); got != v.exact {
			t.Errorf("wildmatch(%q, %q) = %v, want %v", v.pat, v.text, got, v.exact)
		}
		if got := wildmatch(v.pat, v.text, true); got != v.fold {
			t.Errorf("wildmatch fold(%q, %q) = %v, want %v", v.pat, v.text, got, v.fold)
		}
	}
}

func compile(t *testing.T, fold bool, lines ...string) *Matcher {
	t.Helper()
	m, errs := Compile(lines, fold)
	if len(errs) != 0 {
		t.Fatalf("Compile(%q): %v", lines, errs)
	}
	return m
}

func TestExcludedPath(t *testing.T) {
	tests := []struct {
		rules []string
		path  string
		isDir bool
		want  bool
	}{
		{[]string{"*.log"}, "a/b/c.log", false, true},
		{[]string{"/build"}, "a/build", true, false},
		{[]string{"/build"}, "build", true, true},
		{[]string{"node_modules/"}, "x/node_modules", true, true},
		{[]string{"node_modules/"}, "x/node_modules", false, false},
		{[]string{"a/**/b"}, "a/b", false, true},
		{[]string{"a/**/b"}, "a/x/y/b", false, true},
		// A parent directory excluded: "!" cannot re-include its content.
		{[]string{"build/", "!build/keep.txt"}, "build/keep.txt", false, true},
		{[]string{"build", "!build/keep.txt"}, "build/keep.txt", false, true},
		// "/*" with "!/Docs/" keeps only Docs.
		{[]string{"/*", "!/Docs/"}, "Docs", true, false},
		{[]string{"/*", "!/Docs/"}, "Docs/a/b.txt", false, false},
		{[]string{"/*", "!/Docs/"}, "Other", true, true},
		{[]string{"/*", "!/Docs/"}, "Other/x.txt", false, true},
		{[]string{"/*", "!/Docs/"}, "top.txt", false, true},
		// Last match wins, escapes, trailing spaces, comments.
		{[]string{"*.iso", "!/Images/*.iso"}, "Images/a.iso", false, false},
		{[]string{"*.iso", "!/Images/*.iso"}, "Other/a.iso", false, true},
		{[]string{"\\#file"}, "#file", false, true},
		{[]string{"#file"}, "#file", false, false},
		{[]string{"\\!x"}, "!x", false, true},
		{[]string{"a b  "}, "a b", false, true},
		{[]string{"a\\ "}, "a ", false, true},
		{[]string{"a\\ "}, "a", false, false},
		{[]string{"photo-??.raw"}, "photo-01.raw", false, true},
		{[]string{"photo-??.raw"}, "photo-1.raw", false, false},
	}
	for _, tc := range tests {
		m := compile(t, false, tc.rules...)
		if got := m.ExcludedPath(tc.path, tc.isDir); got != tc.want {
			t.Errorf("rules %q path %q dir=%v: got %v, want %v", tc.rules, tc.path, tc.isDir, got, tc.want)
		}
	}
}

func TestCaseFold(t *testing.T) {
	m := compile(t, true, "*.ISO", "/Build/")
	for _, p := range []string{"a/x.iso", "A/X.Iso"} {
		if !m.Excluded(p, false) {
			t.Errorf("%q should be excluded", p)
		}
	}
	if !m.Excluded("build", true) {
		t.Error("build/ should match /Build/ when folding")
	}
	if compile(t, false, "*.ISO").Excluded("x.iso", false) {
		t.Error("case-sensitive matcher matched x.iso")
	}
}

func TestLineErrors(t *testing.T) {
	m, errs := Compile([]string{"ok", "bad\\", "# c", "a[b", "[[:nope:]]", "fine"}, false)
	if len(errs) != 3 || errs[0].Line != 2 || errs[1].Line != 4 || errs[2].Line != 5 {
		t.Fatalf("errs = %+v", errs)
	}
	if !m.Excluded("ok", false) || !m.Excluded("fine", false) || m.Excluded("a[b", false) {
		t.Error("invalid lines must not take part in matching")
	}
}

func TestPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("performance test")
	}
	var lines []string
	for i := 0; i < 100; i++ {
		switch i % 4 {
		case 0:
			lines = append(lines, fmt.Sprintf("*.ext%d", i))
		case 1:
			lines = append(lines, fmt.Sprintf("/dir%d/**/tmp", i))
		case 2:
			lines = append(lines, fmt.Sprintf("cache%d/", i))
		default:
			lines = append(lines, fmt.Sprintf("!keep-%d-??.dat", i))
		}
	}
	m := compile(t, false, lines...)
	paths := make([]string, 100000)
	for i := range paths {
		paths[i] = fmt.Sprintf("dir%d/sub%d/file%d.txt", i%500, i%97, i)
	}
	start := time.Now()
	n := 0
	for _, p := range paths {
		if m.Excluded(p, false) {
			n++
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("100k paths x 100 rules took %v (%d excluded)", d, n)
	}
}
