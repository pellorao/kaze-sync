package proto

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

// A buffer smaller than the block the sender announces is an error, not a slice panic: callers size the buffer
// to the file they expect.
func TestReadBlockSmallBuffer(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 100)
	var wire bytes.Buffer
	if err := WriteBlock(&wire, sha256.Sum256(data), data); err != nil {
		t.Fatal(err)
	}
	raw := wire.Bytes()
	if _, _, err := ReadBlock(bytes.NewReader(raw), make([]byte, 50)); err == nil {
		t.Fatal("a block longer than the buffer was accepted")
	}
	if _, got, err := ReadBlock(bytes.NewReader(raw), make([]byte, 100)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("exact buffer: %v", err)
	}
}
