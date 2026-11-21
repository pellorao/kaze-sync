package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
)

// File is one binary of an installer payload.
type File struct {
	Name string
	Data []byte
}

// Build assembles an installer: the executable prefix, a gzip tar payload with the zero-timestamp header that
// FindPayload looks for, the manifest (its digest is filled in) and, when reg is not nil, the registration trailer.
func Build(prefix []byte, files []File, m Manifest, reg *Registration) ([]byte, error) {
	var payload bytes.Buffer
	gz := gzip.NewWriter(&payload)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: 0o755, Size: int64(len(f.Data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload.Bytes())
	m.PayloadSHA256 = hex.EncodeToString(sum[:])
	mt, err := MarshalTrailer(m, ManifestMagic)
	if err != nil {
		return nil, err
	}
	out := append(append(append([]byte(nil), prefix...), payload.Bytes()...), mt...)
	if reg != nil {
		rt, err := MarshalTrailer(reg, RegistrationMagic)
		if err != nil {
			return nil, err
		}
		out = append(out, rt...)
	}
	return out, nil
}
