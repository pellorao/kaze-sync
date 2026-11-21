// Package installer defines the installer file trailers shared by release tools, servers and clients.
package installer

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"kaze-sync/internal/version"
)

const (
	ManifestMagic     = "KZPAYLD1"
	RegistrationMagic = "KZSYNC01"
	footerSize        = 16
	maxTrailerSize    = 4096
)

type Manifest struct {
	Version       string `json:"version"`
	AgentVersion  int    `json:"agent_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	PayloadSHA256 string `json:"payload_sha256"`
}

type Registration struct {
	Server      string `json:"server"`
	Fingerprint string `json:"fingerprint"`
	Code        string `json:"code"`
	Name        string `json:"name"`
}

// MarshalTrailer encodes a JSON record followed by its little-endian length and eight-byte marker.
func MarshalTrailer(record any, magic string) ([]byte, error) {
	if len(magic) != 8 {
		return nil, errors.New("invalid installer trailer marker")
	}
	b, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(b) > maxTrailerSize {
		return nil, errors.New("installer trailer is too large")
	}
	b = binary.LittleEndian.AppendUint64(b, uint64(len(b)))
	return append(b, magic...), nil
}

// ReadManifest reads the small manifest at the end of an installer with no registration trailer.
// It returns the end offset of the compressed payload.
func ReadManifest(r io.ReaderAt, size int64) (Manifest, int64, error) {
	var m Manifest
	var footer [footerSize]byte
	if size < int64(len(footer)) {
		return m, 0, errors.New("installer has no manifest")
	}
	if _, err := r.ReadAt(footer[:], size-int64(len(footer))); err != nil {
		return m, 0, err
	}
	if string(footer[8:]) != ManifestMagic {
		return m, 0, errors.New("installer has no manifest")
	}
	n := binary.LittleEndian.Uint64(footer[:8])
	if n == 0 || n > maxTrailerSize || n >= uint64(size-int64(len(footer))) {
		return m, 0, errors.New("invalid installer manifest length")
	}
	end := size - int64(len(footer)) - int64(n)
	body := make([]byte, n)
	if _, err := r.ReadAt(body, end); err != nil {
		return m, 0, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, 0, fmt.Errorf("invalid installer manifest: %w", err)
	}
	digest, err := hex.DecodeString(m.PayloadSHA256)
	if !version.Valid(m.Version) || m.AgentVersion < 1 || err != nil || len(digest) != 32 {
		return m, 0, errors.New("invalid installer version, agent_version or payload_sha256")
	}
	return m, end, nil
}
