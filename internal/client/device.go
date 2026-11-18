// Package client is the sync client: enrollment, the per-folder engine
// (Scan) and the agent loop.
package client

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// Device is the enrollment information in device.json (design 3.1).
type Device struct {
	Server      string `json:"server"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Token       string `json:"token"`
	Name        string `json:"name"` // the device name; a rename on the server rewrites it (Agent.saveName)
	DeviceID    int64  `json:"device_id"`
}

// StateDir is where the client keeps device.json, config.json, folder
// databases and the log.
func StateDir() (string, error) {
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("LOCALAPPDATA is not set")
		}
		return filepath.Join(base, "kaze-sync"), nil
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "kaze-sync"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "kaze-sync"), nil
}

// writeJSON writes v atomically (temp file, then rename) with mode 0600.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func SaveDevice(dir string, d Device) error { return writeJSON(filepath.Join(dir, "device.json"), d) }

func LoadDevice(dir string) (Device, error) {
	var d Device
	err := readJSON(filepath.Join(dir, "device.json"), &d)
	return d, err
}
