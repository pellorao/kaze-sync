package server

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The count badge sticks out at the right of the bell (right: -Npx) with a ring of its own; the bell and the settings button
// are only the top bar's gap apart. The badge, ring included, must not reach into the settings button (user 2026-11-15: the
// "12" touched the gear): the overhang may not exceed the gap plus the extra room the bell keeps at its right. The badge is
// anchored at its right edge, so a wider count (2 digits, 99+) grows to the left and the overhang does not change.
func TestBadgeStaysOffTheNextButton(t *testing.T) {
	t.Parallel()
	cssb, _ := fs.ReadFile(webFS, "web/style.css")
	css := string(cssb)
	num := func(re string) int {
		m := regexp.MustCompile(re).FindStringSubmatch(css)
		if m == nil {
			t.Fatalf("style.css: no match for %s", re)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	right := -num(`(?m)^\.badge \{[^}]*right: (-\d+)px`)
	ring := num(`(?m)^\.badge \{[^}]*box-shadow: 0 0 0 (\d+)px`)
	gap := num(`(?m)^\.bar-in \{[^}]*gap: (\d+)px`)
	extra := num(`(?m)^\.icon-btn\.bell \{[^}]*margin-right: (\d+)px`)
	if over := right + ring - 1; over >= gap+extra { // right is measured from the padding box of the bell, inside its 1px border
		t.Errorf("the badge (with its ring) sticks out %dpx at the right of the bell, the room to the next button is %dpx (gap %d + margin %d): it touches or overlaps the button", over, gap+extra, gap, extra)
	}
	if !regexp.MustCompile(`(?m)^\.badge \{[^}]*right: -\d+px`).MatchString(css) || strings.Contains(css, ".badge { position: absolute; top: -5px; left") {
		t.Error("the badge is no longer anchored by its right edge")
	}
	layout, _ := fs.ReadFile(webFS, "web/layout.html")
	if !regexp.MustCompile(`<button class="icon-btn bell"[^>]*popovertarget="notif"`).Match(layout) {
		t.Error("the bell button of layout.html does not carry the bell class")
	}
}
