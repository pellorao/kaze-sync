// Package cmd_test checks the command lines of both binaries: help and
// malformed arguments must print the usage and exit without starting anything.
package cmd_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func build(t *testing.T, name string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), name)
	if out, err := exec.Command("go", "build", "-o", exe, "./"+name).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
	}
	return exe
}

// execute runs exe in an empty directory with every state location pointing there.
func execute(t *testing.T, exe string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_STATE_HOME="+dir, "LOCALAPPDATA="+dir)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%v did not exit (something was started)", args)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	if left, _ := os.ReadDir(dir); len(left) > 0 {
		t.Errorf("%v created files in the state directory: %v", args, left[0].Name())
	}
	return o.String(), e.String(), code
}

func TestCommandLines(t *testing.T) {
	cases := []struct {
		bin  string
		cmds []string // subcommands taking part in the checks
	}{
		{"kaze-syncc", []string{"enroll", "run"}},
		{"kaze-syncd", []string{"run", "install", "admin-password", "export"}},
	}
	for _, c := range cases {
		exe := build(t, c.bin)
		var invocations [][]string
		for _, h := range []string{"-h", "--help", "-help"} {
			invocations = append(invocations, []string{h}, []string{"help"})
			for _, sub := range c.cmds {
				invocations = append(invocations, []string{sub, h})
			}
		}
		for _, args := range invocations {
			out, errOut, code := execute(t, exe, args...)
			if code != 0 || !strings.Contains(out, "usage:") || errOut != "" {
				t.Errorf("%s %v: code %d, stdout %q, stderr %q; want usage on stdout and exit 0", c.bin, args, code, out, errOut)
			}
		}
		bad := [][]string{{}, {"bogus"}, {"--bogus"}}
		for _, sub := range c.cmds {
			bad = append(bad, []string{sub, "--bogus"}, []string{sub, "-x"})
		}
		bad = append(bad, []string{"run", "stray"}, []string{"run", "--data", "/nonexistent", "stray"})
		for _, args := range bad {
			_, errOut, code := execute(t, exe, args...)
			if code != 2 || !strings.Contains(errOut, "usage:") {
				t.Errorf("%s %v: code %d, stderr %q; want usage on stderr and exit 2", c.bin, args, code, errOut)
			}
		}
	}
}
