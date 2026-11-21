// Package cmd_test checks the command lines of both binaries: help and
// malformed arguments must print the usage and exit without starting anything.
package cmd_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"kaze-sync/internal/installer"
	"kaze-sync/internal/version"
)

var binaries = map[string]string{"kaze-syncc": "", "kaze-syncd": ""} // name -> built executable

// TestMain builds both binaries once, side by side, for every test of the package.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kaze-cmd-test")
	if err != nil {
		panic(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := false
	var names []string // a fixed list: the workers write to the map while it would still be iterated
	for name := range binaries {
		names = append(names, name)
	}
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exe := filepath.Join(dir, name)
			out, err := exec.Command("go", "build", "-o", exe, "./"+name).CombinedOutput()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = true
				os.Stderr.WriteString("build " + name + ": " + err.Error() + "\n" + string(out))
			}
			binaries[name] = exe
		}()
	}
	wg.Wait()
	code := 1
	if !failed {
		code = m.Run()
	}
	os.RemoveAll(dir)
	os.Exit(code)
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
		{"kaze-syncc", []string{"enroll", "run", "install"}},
		{"kaze-syncd", []string{"run", "install", "admin-password", "export"}},
	}
	for _, c := range cases {
		exe := binaries[c.bin]
		var invocations [][]string
		for _, h := range []string{"-h", "--help", "-help"} {
			invocations = append(invocations, []string{h}, []string{"help"})
			for _, sub := range c.cmds {
				invocations = append(invocations, []string{sub, h})
			}
		}
		for _, args := range invocations {
			t.Run(c.bin+"/"+strings.Join(args, "_"), func(t *testing.T) {
				t.Parallel()
				out, errOut, code := execute(t, exe, args...)
				if code != 0 || !strings.Contains(out, "usage:") || errOut != "" {
					t.Errorf("%s %v: code %d, stdout %q, stderr %q; want usage on stdout and exit 0", c.bin, args, code, out, errOut)
				}
			})
		}
		bad := [][]string{{}, {"bogus"}, {"--bogus"}}
		for _, sub := range c.cmds {
			bad = append(bad, []string{sub, "--bogus"}, []string{sub, "-x"})
		}
		bad = append(bad, []string{"run", "stray"}, []string{"run", "--data", "/nonexistent", "stray"})
		for _, args := range bad {
			t.Run(c.bin+"/bad_"+strings.Join(args, "_"), func(t *testing.T) {
				t.Parallel()
				_, errOut, code := execute(t, exe, args...)
				if code != 2 || !strings.Contains(errOut, "usage:") {
					t.Errorf("%s %v: code %d, stderr %q; want usage on stderr and exit 2", c.bin, args, code, errOut)
				}
			})
		}
	}
}

// --version prints the one version both binaries are built with and starts nothing.
func TestVersionFlag(t *testing.T) {
	for _, bin := range []string{"kaze-syncc", "kaze-syncd"} {
		out, _, code := execute(t, binaries[bin], "--version")
		if code != 0 || strings.TrimSpace(out) != bin+" "+version.Version {
			t.Errorf("%s --version: code %d, %q", bin, code, out)
		}
	}
}

// install runs end to end as a process: first installation, then an upgrade that finds the old version. The
// machine is not enrolled and has no autostart record, so no service is touched.
func TestInstallCommand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake program is a shell script")
	}
	home := t.TempDir()
	pkg := func(v string) string {
		prog := "#!/bin/sh\necho kaze-syncc " + v + "\n"
		b, err := installer.Build([]byte("#!/bin/sh\n"), []installer.File{{Name: "kaze-syncc", Data: []byte(prog)}},
			installer.Manifest{Version: v, AgentVersion: 6, OS: "linux", Arch: "amd64"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "kaze-sync-client.sh")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	install := func(v string) string {
		cmd := exec.Command(binaries["kaze-syncc"], "install", "--from", pkg(v))
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("install %s: %v\n%s", v, err, out)
		}
		return string(out)
	}
	if out := install("0.2.0"); !strings.Contains(out, "none -> 0.2.0") {
		t.Fatalf("first install: %s", out)
	}
	if out := install("0.3.0"); !strings.Contains(out, "0.2.0 -> 0.3.0") {
		t.Fatalf("upgrade: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".local/bin/kaze-syncc")); !strings.Contains(string(b), "0.3.0") {
		t.Fatalf("installed program: %q", b)
	}
}
