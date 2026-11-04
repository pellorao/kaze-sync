package ignore

import "strings"

// Port of Git's wildmatch.c (WM_PATHNAME always set, WM_CASEFOLD optional).
// Like Git it works on bytes and folds ASCII case only. Indexes past the end
// read as 0, mirroring the NUL terminator of the C strings.

const (
	nomatch = iota
	match
	abortAll
	abortToStarStar
)

func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }

func lower(c byte) byte {
	if isUpper(c) {
		return c + 'a' - 'A'
	}
	return c
}

func wildmatch(pat, text string, fold bool) bool {
	return dowild(pat, 0, text, 0, fold) == match
}

func dowild(p string, pi int, t string, ti int, fold bool) int {
	start := pi
	for ; at(p, pi) != 0; pi, ti = pi+1, ti+1 {
		pc, tc := at(p, pi), at(t, ti)
		if tc == 0 && pc != '*' {
			return abortAll
		}
		if fold {
			tc, pc = lower(tc), lower(pc)
		}
		switch pc {
		case '?':
			if tc == '/' {
				return nomatch
			}
		case '*':
			pi++
			matchSlash := false
			if at(p, pi) == '*' {
				prev := pi - 2
				for pi++; at(p, pi) == '*'; pi++ {
				}
				if (prev < start || p[prev] == '/') &&
					(at(p, pi) == 0 || at(p, pi) == '/' || (at(p, pi) == '\\' && at(p, pi+1) == '/')) {
					// "**/" may match nothing: try the rest of the pattern here.
					if at(p, pi) == '/' && dowild(p, pi+1, t, ti, fold) == match {
						return match
					}
					matchSlash = true
				}
			}
			if at(p, pi) == 0 {
				// A trailing "*" must not cross a slash, a trailing "**" may.
				if !matchSlash && strings.IndexByte(t[ti:], '/') >= 0 {
					return nomatch
				}
				return match
			}
			if !matchSlash && at(p, pi) == '/' {
				// One "*" followed by "/" matches up to the next slash; the
				// loop post statement consumes the slash on both sides.
				slash := strings.IndexByte(t[ti:], '/')
				if slash < 0 {
					return nomatch
				}
				ti += slash
				break
			}
			for tc != 0 {
				// Skip ahead to the next candidate when a literal follows.
				if c := at(p, pi); c != '*' && c != '?' && c != '[' && c != '\\' {
					if fold {
						c = lower(c)
					}
					for {
						tc = at(t, ti)
						if tc == 0 || (!matchSlash && tc == '/') {
							break
						}
						if fold {
							tc = lower(tc)
						}
						if tc == c {
							break
						}
						ti++
					}
					if tc != c {
						return nomatch
					}
				}
				if m := dowild(p, pi, t, ti, fold); m != nomatch {
					if !matchSlash || m != abortToStarStar {
						return m
					}
				} else if !matchSlash && tc == '/' {
					return abortToStarStar
				}
				ti++
				tc = at(t, ti)
			}
			return abortAll
		case '[':
			ok, end, valid := class(p, pi, tc, fold)
			if !valid {
				return abortAll
			}
			if !ok || tc == '/' {
				return nomatch
			}
			pi = end
		default:
			if pc == '\\' {
				pi++
				pc = at(p, pi)
			}
			if tc != pc {
				return nomatch
			}
		}
	}
	if at(t, ti) != 0 {
		return nomatch
	}
	return match
}

// class evaluates the bracket expression starting at p[i] == '[' against tc.
// It returns whether tc matches (negation applied), the index of the closing
// ']', and false for valid when the expression is malformed.
func class(p string, i int, tc byte, fold bool) (ok bool, end int, valid bool) {
	i++
	pc := at(p, i)
	negated := pc == '!' || pc == '^'
	if negated {
		i++
		pc = at(p, i)
	}
	var prev byte
	matched := false
	for {
		if pc == 0 {
			return
		}
		switch {
		case pc == '\\':
			i++
			if pc = at(p, i); pc == 0 {
				return
			}
			matched = matched || tc == pc
		case pc == '-' && prev != 0 && at(p, i+1) != 0 && at(p, i+1) != ']':
			i++
			if pc = at(p, i); pc == '\\' {
				i++
				if pc = at(p, i); pc == 0 {
					return
				}
			}
			if tc <= pc && tc >= prev {
				matched = true
			} else if fold && isLower(tc) {
				if u := tc - 'a' + 'A'; u <= pc && u >= prev {
					matched = true
				}
			}
			pc = 0
		case pc == '[' && at(p, i+1) == ':':
			s := i + 2
			j := s
			for at(p, j) != 0 && at(p, j) != ']' {
				j++
			}
			if at(p, j) == 0 {
				return
			}
			if j == s || p[j-1] != ':' {
				// No ":]": treat '[' as an ordinary member.
				i = s - 2
				pc = '['
				matched = matched || tc == pc
				break
			}
			in, known := inClass(p[s:j-1], tc, fold)
			if !known {
				return
			}
			matched = matched || in
			i = j
			pc = 0
		default:
			matched = matched || tc == pc
		}
		prev = pc
		i++
		if pc = at(p, i); pc == ']' {
			return matched != negated, i, true
		}
	}
}

func inClass(name string, c byte, fold bool) (in, known bool) {
	alpha := isLower(c) || isUpper(c)
	digit := c >= '0' && c <= '9'
	switch name {
	case "alnum":
		return alpha || digit, true
	case "alpha":
		return alpha, true
	case "blank":
		return c == ' ' || c == '\t', true
	case "cntrl":
		return c < 32 || c == 127, true
	case "digit":
		return digit, true
	case "graph":
		return c > 32 && c < 127, true
	case "lower":
		return isLower(c), true
	case "print":
		return c >= 32 && c < 127, true
	case "punct":
		return c > 32 && c < 127 && !alpha && !digit, true
	case "space":
		return c == ' ' || c == '\t' || c == '\n' || c == '\r', true
	case "upper":
		return isUpper(c) || (fold && isLower(c)), true
	case "xdigit":
		return digit || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'), true
	}
	return false, false
}

// The messages of LineError, English; the Web UI knows them and shows them in its own language.
const (
	ErrTrailingBackslash = "trailing backslash"
	ErrBracket           = "unterminated or invalid bracket expression"
)

// malformed reports why pat can never match ("" if it is well formed).
func malformed(pat string) string {
	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '\\':
			i++
			if i == len(pat) {
				return ErrTrailingBackslash
			}
		case '[':
			_, end, valid := class(pat, i, 0, false)
			if !valid {
				return ErrBracket
			}
			i = end
		}
	}
	return ""
}
