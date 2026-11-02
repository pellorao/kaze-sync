package client

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"

	"kaze-sync/internal/proto"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Enroll exchanges the one-time code for a device token and writes device.json to dir.
func Enroll(ctx context.Context, dir, server, code, fingerprint string) error {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("server must be an https URL, got %q", server)
	}
	hc, err := NewHTTPClient(fingerprint)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	api := &API{Base: server, HC: hc}
	resp, err := api.Enroll(ctx, proto.EnrollReq{Code: code, OS: runtime.GOOS, Hostname: host})
	if err != nil {
		return err
	}
	return SaveDevice(dir, Device{Server: server, Fingerprint: fingerprint, Token: resp.Token, Name: resp.Name, DeviceID: resp.DeviceID})
}
