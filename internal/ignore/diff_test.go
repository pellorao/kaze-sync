package ignore

import (
	"bytes"
	"math/rand/v2"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const diffSeed = 20261101

var diffNames = []string{"a", "b", "ab", "A", "B", "a.txt", "b.log", "X.LOG", "x y", "[x]", "build", "Docs", "tmp", "a-b", "k1"}

var diffTokens = []string{
	"a", "b", "ab", "A", "build", "Docs", "tmp", "*", "**", "?", "*.txt", "*.log", "a*", "*b", "[a-c]", "[!a]", "[^a]",
	"[[:upper:]]", "[[:alpha:]]*", "\\*", "\\[x]", "[x]", "x\\ y", "k?", "**b**",
}

type diffEntry struct {
	path  string
	isDir bool
}

// diffTree creates a small directory tree under root and returns its entries.
func diffTree(t *testing.T, root string, rng *rand.Rand, unique bool) []diffEntry {
	t.Helper()
	var entries []diffEntry
	seen := map[string]bool{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		n := 3 + rng.IntN(4)
		used := map[string]bool{}
		for i := 0; i < n; i++ {
			name := diffNames[rng.IntN(len(diffNames))]
			key := name
			if unique {
				key = strings.ToLower(name)
			}
			if used[key] {
				continue
			}
			used[key] = true
			p := path.Join(dir, name)
			if seen[p] {
				continue
			}
			seen[p] = true
			isDir := depth < 3 && rng.IntN(3) == 0
			entries = append(entries, diffEntry{p, isDir})
			if isDir {
				if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
					t.Fatal(err)
				}
				walk(p, depth+1)
			} else if err := os.WriteFile(filepath.Join(root, p), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	walk("", 0)
	return entries
}

func diffRules(rng *rand.Rand) []string {
	var lines []string
	for n := 1 + rng.IntN(5); n > 0; n-- {
		var segs []string
		for k := 1 + rng.IntN(3); k > 0; k-- {
			segs = append(segs, diffTokens[rng.IntN(len(diffTokens))])
		}
		line := strings.Join(segs, "/")
		switch rng.IntN(4) {
		case 0:
			line = "/" + line
		case 1:
			line += "/"
		}
		if rng.IntN(4) == 0 {
			line = "!" + line
		}
		lines = append(lines, line)
	}
	return lines
}

// Over 100 ms (serial run): compares at least 5000 pairs against a real git process (check-ignore), per case.
func TestDifferentialAgainstGit(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("differential test runs on Linux only")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, fold := range []bool{false, true} {
		t.Run(map[bool]string{true: "fold", false: "exact"}[fold], func(t *testing.T) { t.Parallel(); diffAgainstGit(t, fold) })
	}
}

func diffAgainstGit(t *testing.T, fold bool) {
	{
		rng := rand.New(rand.NewPCG(diffSeed, 1))
		root := t.TempDir()
		git := func(stdin []byte, args ...string) []byte {
			args = append([]string{"-C", root, "-c", "core.ignoreCase=" + map[bool]string{true: "true", false: "false"}[fold]}, args...)
			cmd := exec.Command("git", args...)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
			cmd.Stdin = bytes.NewReader(stdin)
			out, err := cmd.Output()
			if ee, ok := err.(*exec.ExitError); err != nil && !(ok && ee.ExitCode() == 1) {
				t.Fatalf("git %v: %v", args, err)
			}
			return out
		}
		git(nil, "init", "-q")
		entries := diffTree(t, root, rng, fold)
		var stdin []byte
		for _, e := range entries {
			stdin = append(stdin, e.path+"\x00"...)
		}
		pairs, hits := 0, 0
		for iter := 0; pairs < 5000 || iter < 100; iter++ {
			lines := diffRules(rng)
			m, errs := Compile(lines, fold)
			if len(errs) != 0 {
				t.Fatalf("generated invalid rules %q: %v", lines, errs)
			}
			if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var ignored []string
			for _, p := range bytes.Split(git(stdin, "check-ignore", "--no-index", "-z", "--stdin"), []byte{0}) {
				ignored = append(ignored, string(p))
			}
			for _, e := range entries {
				pairs++
				if slices.Contains(ignored, e.path) {
					hits++
				}
				if got, want := m.ExcludedPath(e.path, e.isDir), slices.Contains(ignored, e.path); got != want {
					t.Errorf("fold=%v rules %q path %q dir=%v: got %v, git says %v", fold, lines, e.path, e.isDir, got, want)
				}
			}
			if t.Failed() {
				break
			}
		}
		t.Logf("fold=%v: %d (ruleset, path) pairs compared, %d ignored", fold, pairs, hits)
	}
}
