package version

import (
	"os"
	"regexp"
	"testing"
)

var semver = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)

func TestVersionIsSemver(t *testing.T) {
	if !semver.MatchString(Version) {
		t.Fatalf("Version %q is not MAJOR.MINOR.PATCH", Version)
	}
}

// A release raises Version and turns the Unreleased changelog section into the heading of that version; forgetting one of
// the two fails here. Release notes carry the version in their title.
func TestVersionMatchesChangelogAndNotes(t *testing.T) {
	for _, c := range []struct{ file, re string }{
		{"CHANGELOG.md", `(?m)^## v(\S+) - \d{4}-\d{2}-\d{2}$`},
		{"CHANGELOG.zh-CN.md", `(?m)^## v(\S+) - \d{4}-\d{2}-\d{2}$`},
		{"RELEASE-NOTES.md", `(?m)^# kaze-sync v(\S+) release notes$`},
		{"RELEASE-NOTES.zh-CN.md", `(?m)^# kaze-sync v(\S+) 发布说明$`},
	} {
		b, err := os.ReadFile("../../" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(c.re).FindSubmatch(b)
		if m == nil {
			t.Errorf("%s: no version heading matching %s", c.file, c.re)
		} else if string(m[1]) != Version {
			t.Errorf("%s: newest version is %s, Version is %s", c.file, m[1], Version)
		}
	}
}
