package server

import "testing"

func TestGlobFilter(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"*.md", "a/b/Notes.MD", true},
		{"*.md", "a/b/notes.md.txt", false},
		{"notes.md", "a/notes.md", true},
		{"a/*.md", "a/x.md", true},
		{"a/*.md", "a/b/x.md", false},
		{"a/**", "a/b/c/x.md", true},
		{"a/**/x.md", "a/b/c/x.md", true},
		{"f0?", "d/f01", true},
		{"f0?", "d/f012", false},
		{"a.b", "axb", false},
		{"(x)+", "(x)+", true},
		{"é*", "é.txt", true},
		{"é*", "Ã©.txt", false},
		{"日?.md", "d/日本.md", true},
	}
	for _, c := range cases {
		if got := globFilter(c.glob)(c.path); got != c.want {
			t.Errorf("%q on %q: %v, want %v", c.glob, c.path, got, c.want)
		}
	}
	if globFilter("  ") != nil {
		t.Error("an empty glob keeps everything")
	}
}

func TestHitsViewFilter(t *testing.T) {
	var hits []ruleHit
	for _, p := range []string{"a/1.log", "a/2.log", "b/3.txt"} {
		hits = append(hits, ruleHit{Folder: 1, Name: "Docs", Path: p})
	}
	v := newHitsView("x", hits, true, "*.log")
	if v.Total != 3 || v.Match != 2 || !v.Filtered || len(v.Paths) != 2 || v.Paths[0] != "Docs: a/1.log" {
		t.Errorf("%+v", v)
	}
	if v.Token != newHitsView("x", hits, true, "").Token {
		t.Error("the token must not depend on the filter")
	}
	for i := 0; i < 80; i++ {
		hits = append(hits, ruleHit{Path: "z/f"})
	}
	if v := newHitsView("x", hits, false, ""); v.Match != 83 || len(v.Paths) != previewMax {
		t.Errorf("%d %d", v.Match, len(v.Paths))
	}
}
