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
	"net/http"
	"strings"
	"time"

	"kaze-sync/internal/proto"
)

var ErrUnauthorized = errors.New("server rejected the device token (revoked? enroll again)")

// NewHTTPClient returns a client for the server. With a fingerprint
// ("sha256:<hex>") only the certificate with that hash is accepted, whatever
// its name; without one the system roots apply. HTTPS_PROXY is honoured.
func NewHTTPClient(fingerprint string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if fingerprint != "" {
		want, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(fingerprint), "sha256:"))
		if err != nil || len(want) != sha256.Size {
			return nil, fmt.Errorf("invalid fingerprint %q", fingerprint)
		}
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, // replaced by the pin below
			MinVersion:         tls.VersionTLS12,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 {
					return errors.New("server sent no certificate")
				}
				if sum := sha256.Sum256(raw[0]); !bytes.Equal(sum[:], want) {
					return errors.New("server certificate does not match the pinned fingerprint")
				}
				return nil
			},
		}
	}
	return &http.Client{Transport: tr}, nil
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
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	resp, err := a.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == 401 && a.Token != "" {
			return ErrUnauthorized
		}
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
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
	return r, a.json(ctx, "poll", req, &r)
}

func (a *API) Commit(ctx context.Context, folder int64, ops []proto.Op) ([]proto.Result, error) {
	var r proto.CommitResp
	if err := a.json(ctx, "commit", proto.CommitReq{Folder: folder, Ops: ops}, &r); err != nil {
		return nil, err
	}
	if len(r.Results) != len(ops) {
		return nil, fmt.Errorf("commit: %d results for %d ops", len(r.Results), len(ops))
	}
	return r.Results, nil
}

// UploadBlocks sends a block packet (see proto.WriteBlock).
func (a *API) UploadBlocks(ctx context.Context, packet []byte) error {
	var r map[string]int
	return a.do(ctx, "blocks", bytes.NewReader(packet), "application/octet-stream", &r)
}
