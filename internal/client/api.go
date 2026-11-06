package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/proto"
)

// Timeouts that keep a silent server from holding a request (and with it a whole cycle) for good. The first is
// above the long poll's 55 seconds; the second is how long a block download may go without a byte.
var (
	responseTimeout = 90 * time.Second
	idleTimeout     = 60 * time.Second
)

// trace starts timing a step for the debug log (off by default, see docs/operations.md); the returned function
// logs it with the extra attributes when the step ends.
func trace(msg string, args ...any) func(more ...any) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return func(...any) {}
	}
	t0 := time.Now()
	return func(more ...any) {
		slog.Debug(msg, append(append(args, "ms", time.Since(t0).Milliseconds()), more...)...)
	}
}

var ErrUnauthorized = errors.New("server rejected the device token (revoked? enroll again)")

// UpgradeError is the server's refusal of a client whose version is too old (HTTP 426); the message says what to do.
type UpgradeError struct{ Msg string }

func (e *UpgradeError) Error() string {
	return "this client is too old for the server, upgrade kaze-syncc: " + e.Msg
}

// httpError turns a non-200 answer into an error; the caller closes the body.
func httpError(path string, resp *http.Response, token string) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == http.StatusUpgradeRequired:
		return &UpgradeError{Msg: strings.TrimSpace(string(msg))}
	case resp.StatusCode == 401 && token != "":
		return ErrUnauthorized
	}
	return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
}

// NewHTTPClient returns a client for the server. With a fingerprint
// ("sha256:<hex>") only the certificate with that hash, or one signed by the
// CA certificate with that hash, is accepted, whatever its name; without one
// the system roots apply. HTTPS_PROXY is honoured.
func NewHTTPClient(fingerprint string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = responseTimeout
	if fingerprint != "" {
		want, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(fingerprint), "sha256:"))
		if err != nil || len(want) != sha256.Size {
			return nil, fmt.Errorf("invalid fingerprint %q", fingerprint)
		}
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify:    true, // replaced by the pin below
			MinVersion:            tls.VersionTLS12,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error { return checkPin(raw, want) },
		}
	}
	return &http.Client{Transport: tr}, nil
}

// checkPin accepts a chain whose first certificate has the pinned hash (self-signed server), or whose
// first certificate is signed by a certificate in the chain with that hash (local CA mode, design 2.5).
// The name is not checked, as for the pinned certificate itself.
func checkPin(raw [][]byte, want []byte) error {
	if len(raw) == 0 {
		return errors.New("server sent no certificate")
	}
	if sum := sha256.Sum256(raw[0]); bytes.Equal(sum[:], want) {
		return nil
	}
	for _, r := range raw[1:] {
		if sum := sha256.Sum256(r); !bytes.Equal(sum[:], want) {
			continue
		}
		ca, err1 := x509.ParseCertificate(r)
		leaf, err2 := x509.ParseCertificate(raw[0])
		if err1 != nil || err2 != nil {
			break
		}
		pool := x509.NewCertPool()
		pool.AddCert(ca)
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return fmt.Errorf("server certificate is not valid under the pinned CA: %w", err)
		}
		return nil
	}
	return errors.New("server certificate does not match the pinned fingerprint")
}

// API talks to the device API of one server.
type API struct {
	Base  string // https://host:port
	Token string
	HC    *http.Client
}

func (a *API) do(ctx context.Context, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(a.Base, "/")+"/api/v1/"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(proto.VersionHeader, strconv.Itoa(proto.AgentVersion))
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	return a.send(req, path, out)
}

func (a *API) send(req *http.Request, path string, out any) error {
	resp, err := a.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return httpError(path, resp, a.Token)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (a *API) json(ctx context.Context, path string, req, out any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return a.do(ctx, path, bytes.NewReader(b), "application/json", out)
}

func (a *API) Enroll(ctx context.Context, req proto.EnrollReq) (proto.EnrollResp, error) {
	var r proto.EnrollResp
	return r, a.json(ctx, "enroll", req, &r)
}

// Poll hangs up to PollTimeoutS seconds; its context should allow a little more.
func (a *API) Poll(ctx context.Context, req proto.PollReq) (proto.PollResp, error) {
	ctx, cancel := context.WithTimeout(ctx, (proto.PollTimeoutS+15)*time.Second)
	defer cancel()
	var r proto.PollResp
	done := trace("poll")
	err := a.json(ctx, "poll", req, &r)
	done("cursors", fmt.Sprint(r.Cursors), "config", r.Config != nil, "err", err)
	return r, err
}

func (a *API) Commit(ctx context.Context, folder int64, ops []proto.Op) ([]proto.Result, error) {
	var r proto.CommitResp
	done := trace("server commit", "folder", folder, "ops", len(ops))
	err := a.json(ctx, "commit", proto.CommitReq{Folder: folder, Ops: ops}, &r)
	done("err", err)
	if err != nil {
		return nil, err
	}
	if len(r.Results) != len(ops) {
		return nil, fmt.Errorf("commit: %d results for %d ops", len(r.Results), len(ops))
	}
	return r.Results, nil
}

// Held sends the whole list of held changes of a folder (design 2.8.10).
func (a *API) Held(ctx context.Context, req proto.HeldReq) error {
	var r map[string]any
	return a.json(ctx, "held", req, &r)
}

// Stage sends one segment of the block list of a big file (design 2.4).
func (a *API) Stage(ctx context.Context, req proto.StageReq) (proto.StageResp, error) {
	var r proto.StageResp
	done := trace("stage", "folder", req.Folder, "offset", req.Offset, "blocks", len(req.Blocks))
	err := a.json(ctx, "stage", req, &r)
	done("err", err)
	return r, err
}

// UploadBlocks sends a block packet (see proto.WriteBlock).
func (a *API) UploadBlocks(ctx context.Context, packet []byte) error {
	var r map[string]int
	done := trace("upload blocks", "bytes", len(packet))
	err := a.do(ctx, "blocks", bytes.NewReader(packet), "application/octet-stream", &r)
	done("err", err)
	return err
}

// Changes returns one page of the entries of a folder with seq > since.
func (a *API) Changes(ctx context.Context, folder, since int64) (proto.ChangesResp, error) {
	var r proto.ChangesResp
	done := trace("changes", "folder", folder, "since", since)
	defer func() { done("entries", len(r.Entries)) }()
	u := fmt.Sprintf("%s/api/v1/changes?folder=%d&since=%d", strings.TrimRight(a.Base, "/"), folder, since)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return r, err
	}
	req.Header.Set(proto.VersionHeader, strconv.Itoa(proto.AgentVersion))
	req.Header.Set("Authorization", "Bearer "+a.Token)
	return r, a.send(req, "changes", &r)
}

// GetBlocks requests blocks; the caller reads the packet from the body (see
// proto.ReadBlock) and closes it.
func (a *API) GetBlocks(ctx context.Context, hashes []proto.Hash) (io.ReadCloser, error) {
	b, _ := json.Marshal(proto.BlocksGetReq{Hashes: hashes})
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(a.Base, "/")+"/api/v1/blocks/get", bytes.NewReader(b))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(proto.VersionHeader, strconv.Itoa(proto.AgentVersion))
	req.Header.Set("Authorization", "Bearer "+a.Token)
	done := trace("download blocks", "blocks", len(hashes))
	resp, err := a.HC.Do(req)
	done("err", err)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer cancel()
		defer resp.Body.Close()
		return nil, httpError("blocks/get", resp, a.Token)
	}
	timer := time.AfterFunc(time.Hour, cancel)
	timer.Stop() // runs only while a Read waits for the server, not while the caller is busy with the bytes
	return &idleBody{rc: resp.Body, timer: timer, cancel: cancel}, nil
}

// idleBody cancels the request when a Read gets no byte for idleTimeout.
type idleBody struct {
	rc     io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(idleTimeout)
	n, err := b.rc.Read(p)
	b.timer.Stop()
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	defer b.cancel()
	return b.rc.Close()
}
