package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kaze-sync/internal/proto"
)

func proto0() proto.PollReq { return proto.PollReq{} }

func TestEnrollPinsTheCertificate(t *testing.T) {
	t.Parallel()
	e := newCEnv(t)
	ctx := context.Background()

	if err := Enroll(ctx, t.TempDir(), "http://"+e.ts.Listener.Addr().String(), e.code(), e.fp); err == nil {
		t.Error("http URL accepted")
	}
	bad := "sha256:" + strings.Repeat("0", 64)
	if err := Enroll(ctx, t.TempDir(), e.ts.URL, e.code(), bad); err == nil {
		t.Error("wrong fingerprint accepted")
	}
	if err := Enroll(ctx, t.TempDir(), e.ts.URL, e.code(), "sha256:xyz"); err == nil {
		t.Error("malformed fingerprint accepted")
	}
	// Without a pin the self-signed certificate fails the system root check.
	if err := Enroll(ctx, t.TempDir(), e.ts.URL, e.code(), ""); err == nil {
		t.Error("unpinned self-signed certificate accepted")
	}

	dir := filepath.Join(t.TempDir(), "state")
	code := e.code()
	if err := Enroll(ctx, dir, e.ts.URL, code, strings.ToUpper(e.fp)); err != nil {
		t.Fatal(err)
	}
	dev, err := LoadDevice(dir)
	if err != nil || dev.Token == "" || dev.Server != e.ts.URL || dev.DeviceID == 0 {
		t.Fatalf("device.json: %+v %v", dev, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, "device.json")); fi.Mode().Perm() != 0o600 {
			t.Fatalf("device.json mode %v", fi.Mode())
		}
	}
	// The code was used up.
	if err := Enroll(ctx, t.TempDir(), e.ts.URL, code, e.fp); err == nil {
		t.Error("a code worked twice")
	}
}

func TestStateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	t.Setenv("XDG_STATE_HOME", "/x/state")
	if d, _ := StateDir(); d != "/x/state/kaze-sync" {
		t.Fatal(d)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if d, _ := StateDir(); d != "/home/u/.local/state/kaze-sync" {
		t.Fatal(d)
	}
}
