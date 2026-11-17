package main

import (
	"cmp"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// agent is a kaze-syncc client process with its own state directory.
type agent struct {
	name     string
	exe      string
	runExe   string   // the executable that runs the agent when it differs from exe (Windows: the GUI build)
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
	a.cmd = exec.Command(cmp.Or(a.runExe, a.exe), "run")
	a.cmd.Env = a.environ()
	a.cmd.Stdout, a.cmd.Stderr = f, f
	setProcessGroup(a.cmd)
	if err := a.cmd.Start(); err != nil {
		return err
	}
	log.Printf("started agent %s (pid %d)", a.name, a.cmd.Process.Pid)
	return nil
}

// stop ends the agent; kill is SIGKILL (a crash) instead of SIGTERM.
func (a *agent) stop(kill bool) {
	if a.cmd == nil {
		return
	}
	if a.windows {
		exec.Command("taskkill.exe", "/F", "/IM", filepath.Base(cmp.Or(a.runExe, a.exe))).Run()
	} else {
		signalGroup(a.cmd, kill)
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
	b, _ := os.ReadFile(filepath.Join(a.stateDir(), "kaze-syncc.log"))
	return string(b)
}

// stateDir is the client's state directory (below the state root); the state files go here.
func (a *agent) stateDir() string { return filepath.Join(a.state, "kaze-sync") }
