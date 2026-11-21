// Package installer defines the installer file trailers shared by release tools, servers and clients.
package installer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"

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

// ReadRegistration reads the optional registration trailer at the end of a downloaded installer. It returns nil
// and the unchanged size when there is none, otherwise the record and the size without the trailer. The record is
// untrusted input: callers use it only after Validate.
func ReadRegistration(r io.ReaderAt, size int64) (*Registration, int64, error) {
	var footer [footerSize]byte
	if size < int64(len(footer)) {
		return nil, size, nil
	}
	if _, err := r.ReadAt(footer[:], size-int64(len(footer))); err != nil {
		return nil, size, err
	}
	if string(footer[8:]) != RegistrationMagic {
		return nil, size, nil
	}
	n := binary.LittleEndian.Uint64(footer[:8])
	if n == 0 || n > maxTrailerSize || n >= uint64(size-int64(len(footer))) {
		return nil, size, errors.New("invalid installer registration length")
	}
	end := size - int64(len(footer)) - int64(n)
	body := make([]byte, n)
	if _, err := r.ReadAt(body, end); err != nil {
		return nil, size, err
	}
	var reg Registration
	if err := json.Unmarshal(body, &reg); err != nil {
		return nil, size, fmt.Errorf("invalid installer registration: %w", err)
	}
	return &reg, end, nil
}

var (
	codeFormat        = regexp.MustCompile(`^[A-Za-z0-9-]{4,64}$`)
	fingerprintFormat = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
	deviceNameFormat  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

// Validate checks every field against its own format: the record comes from a file that may have been tampered
// with. The fingerprint may be empty (a server whose certificate the system trusts); the name may be empty.
func (reg Registration) Validate() error {
	u, err := url.Parse(reg.Server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid server address %q", reg.Server)
	}
	if !codeFormat.MatchString(reg.Code) {
		return errors.New("invalid registration code")
	}
	if reg.Fingerprint != "" && !fingerprintFormat.MatchString(reg.Fingerprint) {
		return errors.New("invalid certificate fingerprint")
	}
	if reg.Name != "" && !deviceNameFormat.MatchString(reg.Name) {
		return errors.New("invalid device name")
	}
	return nil
}

// Package is a parsed installer file: the manifest, the optional registration and where the payload lies.
type Package struct {
	Manifest     Manifest
	Registration *Registration // nil when the file carries none
	PayloadStart int64
	PayloadEnd   int64
}

// Open parses the trailers of an installer file and locates its payload (a gzip tar of the binaries).
func Open(r io.ReaderAt, size int64) (*Package, error) {
	reg, rest, err := ReadRegistration(r, size)
	if err != nil {
		return nil, err
	}
	m, end, err := ReadManifest(r, rest)
	if err != nil {
		return nil, err
	}
	start, err := FindPayload(r, end, m.PayloadSHA256)
	if err != nil {
		return nil, err
	}
	return &Package{Manifest: m, Registration: reg, PayloadStart: start, PayloadEnd: end}, nil
}

// Payload returns the compressed payload.
func (p *Package) Payload(r io.ReaderAt) *io.SectionReader {
	return io.NewSectionReader(r, p.PayloadStart, p.PayloadEnd-p.PayloadStart)
}

// FindPayload finds the release's fixed gzip header and verifies the compressed bytes up to end against the
// manifest digest. The marker may also occur inside the executable or the compressed stream, so the digest
// selects the payload. It returns the payload's start offset.
func FindPayload(r io.ReaderAt, end int64, wantHex string) (int64, error) {
	header := []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0} // deflate, no optional fields, zero timestamp
	want, _ := hex.DecodeString(wantHex)
	scan := bufio.NewReaderSize(io.NewSectionReader(r, 0, end), 32*1024)
	offset := int64(0)
	candidates := 0
	for {
		segment, err := scan.Peek(scan.Size())
		for pos := 0; pos+len(header) <= len(segment); {
			i := bytes.Index(segment[pos:], header)
			if i < 0 {
				break
			}
			start := offset + int64(pos+i)
			pos += i + 1
			if start == 0 {
				continue // the installer starts with an executable prefix
			}
			candidates++
			if candidates > 16 {
				return 0, errors.New("too many possible installer payload offsets")
			}
			h := sha256.New()
			if _, err := io.Copy(h, io.NewSectionReader(r, start, end-start)); err != nil {
				return 0, err
			}
			if bytes.Equal(h.Sum(nil), want) {
				return start, nil
			}
		}
		if err != nil {
			if err != io.EOF {
				return 0, err
			}
			break
		}
		step := len(segment) - len(header) + 1
		scan.Discard(step)
		offset += int64(step)
	}
	return 0, errors.New("installer payload SHA-256 mismatch")
}
