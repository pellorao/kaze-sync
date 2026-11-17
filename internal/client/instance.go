package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const instanceLockName = "agent.lock"

// ErrAgentRunning means another agent already holds the instance lock of the configuration directory.
var ErrAgentRunning = errors.New("another kaze-syncc agent is already running for this configuration directory")

// LockInstance takes the single-instance lock of the configuration directory (two agents on one local database
// would fight over it and over uploads) and writes the pid into the lock file. The lock goes when the process
// ends; release drops it earlier.
func LockInstance(dir string) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, instanceLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, ErrAgentRunning
	}
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return func() { unlockFile(f); f.Close() }, nil
}

// StopAgent stops the agent that holds the lock, if any, and waits until the lock is free. A new enrollment
// uses it so that the old agent does not go on with the old token.
func StopAgent(dir string) error {
	path := filepath.Join(dir, instanceLockName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // never ran
	}
	if release, err := LockInstance(dir); err == nil {
		release() // nobody holds it: the pid in the file is stale
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return fmt.Errorf("an agent is running but %s has no usable pid", path)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Kill(); err != nil {
		return err
	}
	for range 50 {
		if release, err := LockInstance(dir); err == nil {
			release()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the old agent (pid %d) did not stop", pid)
}
