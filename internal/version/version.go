// Package version holds the one version number of kaze-sync (Semantic Versioning, MAJOR.MINOR.PATCH). The file VERSION
// is embedded at build time into both binaries; a release raises it (docs/operations.md, "Release").
package version

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var raw string

// Version is the content of the VERSION file without the line end.
var Version = strings.TrimSpace(raw)
