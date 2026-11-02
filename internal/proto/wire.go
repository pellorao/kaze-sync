package proto

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Hash is a SHA-256 value; in JSON it is 64 lowercase hexadecimal digits.
type Hash [32]byte

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

func (h *Hash) UnmarshalText(b []byte) error {
	if len(b) != 2*len(h) {
		return fmt.Errorf("invalid hash %q", b)
	}
	_, err := hex.Decode(h[:], b)
	return err
}

// Limits enforced at the server's entrance.
const (
	MaxJSONBody  = 4 << 20
	MaxBlockBody = 64 << 20
	MaxCommitOps = 1000
	PollTimeoutS = 55
	// AgentVersion is the protocol version of this client, bumped on every change of the device API; clients send it in
	// the VersionHeader of every request and in poll requests. MinAgentVersion is the oldest the server still serves; it is
	// raised only by a change that old clients cannot work with, and the rest of the time stays behind AgentVersion
	// (design 9.3). A request without the header comes from a client older than the header: version 1.
	AgentVersion    = 2
	MinAgentVersion = 2
	VersionHeader   = "Kaze-Agent-Version"
)

type EnrollReq struct {
	Code     string `json:"code"`
	OS       string `json:"os"` // "linux" or "windows"
	Hostname string `json:"hostname"`
}

type EnrollResp struct {
	DeviceID int64  `json:"device_id"`
	Name     string `json:"name"` // the device name, used in conflict copy names
	Token    string `json:"token"`
}

// FolderConfig is one mapping of this device with the effective exclusion rules.
type FolderConfig struct {
	ID    int64    `json:"id"`
	Name  string   `json:"name"`
	Path  string   `json:"path"`
	Adopt bool     `json:"adopt"`
	Rules []string `json:"rules"`
}

type Config struct {
	Version int64          `json:"version"`
	Folders []FolderConfig `json:"folders"`
}

// Error kinds a client reports (design 3.6).
const (
	KindInUse    = "in_use"
	KindRejected = "rejected"
	KindSkipped  = "skipped"
	KindIO       = "io"
	KindFolder   = "folder"
)

type FileError struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Since   int64  `json:"since"` // unix seconds
}

type FolderState struct {
	ID           int64       `json:"id"`
	State        string      `json:"state"` // idle, syncing, error
	LastSyncedAt int64       `json:"last_synced_at"`
	PendingUp    int         `json:"pending_up"`
	PendingDown  int         `json:"pending_down"`
	Errors       []FileError `json:"errors,omitempty"` // at most 100
}

type PollReq struct {
	Cursors       map[int64]int64 `json:"cursors,omitempty"` // folder id -> last seq the client knows
	ConfigVersion int64           `json:"config_version"`
	AgentVersion  int             `json:"agent_version"`
	OS            string          `json:"os"`
	Hostname      string          `json:"hostname"`
	Folders       []FolderState   `json:"folders,omitempty"`
}

type PollResp struct {
	Cursors map[int64]int64 `json:"cursors"`          // folder id -> latest seq, for every folder mapped to the device
	Config  *Config         `json:"config,omitempty"` // present when the client's version differs
}

// Op is one change of a commit. A nil Hash deletes the path.
type Op struct {
	Path    string `json:"path"`
	BaseSeq int64  `json:"base_seq"`
	Hash    *Hash  `json:"hash"`
	Blocks  []Hash `json:"blocks,omitempty"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Exec    bool   `json:"exec"`
}

type CommitReq struct {
	Folder int64 `json:"folder"`
	Ops    []Op  `json:"ops"`
}

// Result statuses.
const (
	StatusOK       = "ok"
	StatusNeed     = "need"
	StatusConflict = "conflict"
	StatusRejected = "rejected"
)

// Reasons of a rejected op.
const (
	ReasonCaseCollision = "case_collision"
	ReasonInvalidName   = "invalid_name"
	ReasonTypeConflict  = "type_conflict"
)

// Entry is the state of a path on the server; a nil Hash is a tombstone.
type Entry struct {
	Path    string `json:"path"`
	Seq     int64  `json:"seq"`
	Hash    *Hash  `json:"hash"`
	Blocks  []Hash `json:"blocks,omitempty"`
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Exec    bool   `json:"exec"`
}

type Result struct {
	Status  string `json:"status"`
	Seq     int64  `json:"seq,omitempty"`     // ok
	Need    []Hash `json:"need,omitempty"`    // need
	Current *Entry `json:"current,omitempty"` // conflict; nil when the path does not exist
	Reason  string `json:"reason,omitempty"`  // rejected
}

type CommitResp struct {
	Results []Result `json:"results"`
}

// ChangesResp is one page of GET /changes: the entries with seq > since in
// seq order. Cursor is the last seq returned (since when there is none).
type ChangesResp struct {
	Entries []Entry `json:"entries"`
	Cursor  int64   `json:"cursor"`
	More    bool    `json:"more"`
}

// Limits of GET /changes pages and POST /blocks/get requests.
const (
	MaxChangesPage  = 5000
	MaxChangeBlocks = 20000 // block hashes per page; a page ends early beyond this
	MaxGetBlocks    = MaxBlockBody / BlockSize
)

type BlocksGetReq struct {
	Hashes []Hash `json:"hashes"`
}

// BlockPacket frames one block in a block upload body: 32-byte hash, 4-byte
// big-endian length, data.
func WriteBlock(w io.Writer, h Hash, data []byte) error {
	var hdr [36]byte
	copy(hdr[:], h[:])
	binary.BigEndian.PutUint32(hdr[32:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// ReadBlock reads the next block of a packet; it returns io.EOF at a clean end
// and io.ErrUnexpectedEOF for a truncated block. The data is read into buf
// (at least BlockSize long) and valid until the next call.
func ReadBlock(r io.Reader, buf []byte) (Hash, []byte, error) {
	var hdr [36]byte
	var h Hash
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return h, nil, err
	}
	copy(h[:], hdr[:32])
	n := binary.BigEndian.Uint32(hdr[32:])
	if n > BlockSize {
		return h, nil, errors.New("block too large")
	}
	data := buf[:n]
	if _, err := io.ReadFull(r, data); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return h, nil, err
	}
	return h, data, nil
}
