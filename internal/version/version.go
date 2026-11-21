// Package version holds the one version number of kaze-sync (Semantic Versioning, MAJOR.MINOR.PATCH). The file VERSION
// is embedded at build time into both binaries; a release raises it (docs/operations.md, "Release").
package version

import (
	"cmp"
	_ "embed"
	"fmt"
	"regexp"
	"strings"
)

//go:embed VERSION
var raw string

// Version is the content of the VERSION file without the line end.
var Version = strings.TrimSpace(raw)

var release = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Valid reports whether v has the MAJOR.MINOR.PATCH form used by kaze-sync releases.
func Valid(v string) bool { return release.MatchString(v) }

// Compare orders two kaze-sync release versions numerically.
func Compare(a, b string) (int, error) {
	for _, v := range []string{a, b} {
		if !Valid(v) {
			return 0, fmt.Errorf("invalid kaze-sync release version %q", v)
		}
	}
	bParts := strings.Split(b, ".")
	for i, x := range strings.Split(a, ".") {
		y := bParts[i]
		if n := cmp.Compare(len(x), len(y)); n != 0 {
			return n, nil
		}
		if n := strings.Compare(x, y); n != 0 {
			return n, nil
		}
	}
	return 0, nil
}
