package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// agent is a kaze-syncc client process with its own state directory.
type agent struct {
	name     string
	exe      string
	state    string   // state directory root (XDG_STATE_HOME or, on Windows, LOCALAPPDATA)
	env      []string // extra environment
	windows  bool
	cmd      *exec.Cmd
	logs     string // where stdout/stderr go
	logFile  *os.File
	stateWin string // the Windows spelling of state
}

func (a *agent) environ(extra ...string) []string {
	env := append(os.Environ(), a.env...)
	if a.windows {
		env = append(env, "LOCALAPPDATA="+a.stateWin, "WSLENV=LOCALAPPDATA")
	} else {
		env = append(env, "XDG_STATE_HOME="+a.state)
	}
	return append(env, extra...)
}

func (a *agent) enroll(server, code, fingerprint string) error {
	args := []string{"enroll", server, code, "--no-autostart"}
	if fingerprint != "" {
		args = append(args, "--fingerprint", fingerprint)
	}
	c := exec.Command(a.exe, args...)
	c.Env = a.environ()
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s enroll: %w: %s", a.name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *agent) start() error {
	if a.cmd != nil {
		return fmt.Errorf("%s already running", a.name)
	}
	f, err := os.OpenFile(filepath.Join(a.logs, a.name+".out"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	a.logFile = f
	a.cmd = exec.Command(a.exe, "run")
	a.cmd.Env = a.environ()
	a.cmd.Stdout, a.cmd.Stderr = f, f
	a.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return a.cmd.Start()
}

// stop ends the agent; kill is SIGKILL (a crash) instead of SIGTERM.
func (a *agent) stop(kill bool) {
	if a.cmd == nil {
		return
	}
	if a.windows {
		exec.Command("taskkill.exe", "/F", "/IM", filepath.Base(a.exe)).Run()
	} else if kill {
		syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
	} else {
		syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
	}
	done := make(chan struct{})
	go func() { a.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		a.cmd.Process.Kill()
		<-done
	}
	a.cmd = nil
	a.logFile.Close()
	os.WriteFile(filepath.Join(a.logs, a.name+".state.log"), []byte(a.stateLog()), 0o644)
}

func (a *agent) restart() error {
	a.stop(false)
	return a.start()
}

// stateLog returns the agent's own log file contents (kaze-syncc.log in its state directory).
func (a *agent) stateLog() string {
	dir := a.state
	b, _ := os.ReadFile(filepath.Join(dir, "kaze-sync", "kaze-syncc.log"))
	return string(b)
}
