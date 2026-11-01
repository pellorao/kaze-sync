// Package path holds the rules for file paths shared by server and clients.
// Paths are relative, UTF-8 and separated by "/".
package path

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Key returns the identity of p for collision checks: NFC with simple Unicode case folding.
func Key(p string) string {
	return strings.Map(fold, norm.NFC.String(p))
}

// fold maps r to the smallest rune of its simple case-folding orbit.
func fold(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		m = min(m, f)
	}
	return m
}

// CheckPortable reports why p cannot be created on Windows (or at all): invalid
// UTF-8, an empty segment, forbidden characters, a segment ending in a dot or
// space, or a reserved device name.
func CheckPortable(p string) error {
	if !utf8.ValidString(p) {
		return fmt.Errorf("path %q is not valid UTF-8", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			return fmt.Errorf("path %q has an empty segment", p)
		}
		if i := strings.IndexFunc(seg, func(r rune) bool { return r < 0x20 || strings.ContainsRune(`<>:"\|?*`, r) }); i >= 0 {
			return fmt.Errorf("path %q: segment %q contains forbidden character %q", p, seg, seg[i])
		}
		if last := seg[len(seg)-1]; last == '.' || last == ' ' {
			return fmt.Errorf("path %q: segment %q ends with a dot or space", p, seg)
		}
		if reserved(seg) {
			return fmt.Errorf("path %q: segment %q is a reserved device name", p, seg)
		}
	}
	return nil
}

// reserved reports whether the part of seg before its first dot is a DOS device name.
func reserved(seg string) bool {
	stem, _, _ := strings.Cut(seg, ".")
	stem = strings.ToUpper(stem)
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9'
}
