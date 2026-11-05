package proto

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// HashLen is the size of a Hash in bytes.
const HashLen = 32

// Hash is a SHA-256 value; in JSON it is 64 lowercase hexadecimal digits. It is the one hash type of the
// project: the block store and the garbage collector use it too.
type Hash [HashLen]byte

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

func (h *Hash) UnmarshalText(b []byte) error {
	if len(b) != 2*len(h) {
		return fmt.Errorf("invalid hash %q", b)
	}
	_, err := hex.Decode(h[:], b)
	return err
}

// SplitHashes decodes a concatenation of hashes, the way both databases store a file's block list.
func SplitHashes(b []byte) []Hash {
	hs := make([]Hash, len(b)/HashLen)
	for i := range hs {
		copy(hs[i][:], b[HashLen*i:])
	}
	return hs
}

// JoinHashes is the inverse of SplitHashes.
func JoinHashes(hs []Hash) []byte {
	b := make([]byte, 0, HashLen*len(hs))
	for _, h := range hs {
		b = append(b, h[:]...)
	}
	return b
}

// Limits enforced at the server's entrance.
const (
	MaxJSONBody  = 4 << 20
	MaxBlockBody = 64 << 20
	MaxCommitOps = 1000
	// MaxCommitBlocks bounds the block hashes of one commit: each takes about 67 bytes of JSON, so this stays far below
	// MaxJSONBody. One file with more blocks than this is committed alone.
	MaxCommitBlocks = 20000
	// MaxStageBlocks bounds the block hashes of one /stage segment. MaxFileBlocks bounds the blocks of one file the
	// server accepts (2^19 blocks, 128 GiB): the block list of a file is held whole in the server's memory when it is
	// written or served (design 2.4). MaxStagesPerDevice is how many staged uploads one device may have at once.
	MaxStageBlocks     = 20000
	MaxFileBlocks      = 1 << 19
	MaxStagesPerDevice = 4
	PollTimeoutS       = 55
	// AgentVersion is the protocol version of this client, bumped on every change of the device API; clients send it in
	// the VersionHeader of every request and in poll requests. MinAgentVersion is the oldest the server still serves; it is
	// raised only by a change that old clients cannot work with, and the rest of the time stays behind AgentVersion
	// (design 9.3). Version 5 (symbolic links, design 6.7) stays open for more fields until the next release. A request without the header comes from a client older than the header: version 1.
	AgentVersion    = 5
	MinAgentVersion = 5
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
	Epoch   string          `json:"epoch"`            // identifies the server's history; a client that sees it change reads every folder again
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
	// Link marks a symbolic link: the content is the link's target, verbatim, in one block (design 6.7).
	Link bool `json:"link,omitempty"`
	// Stage names a staged block list (POST /stage) that stands for Blocks when the file has more than
	// MaxCommitBlocks blocks (design 2.4).
	Stage string `json:"stage,omitempty"`
}

// StageReq is one segment of a block list: Blocks[i] is block Offset+i of a file of Size bytes.
type StageReq struct {
	Folder int64  `json:"folder"`
	ID     string `json:"id"`
	Offset int    `json:"offset"`
	Size   int64  `json:"size"`
	Blocks []Hash `json:"blocks"`
}

// StageResp: Have is the number of blocks received so far; Need the hashes of the segment the server does not have;
// a non-empty Reason refuses the segment.
type StageResp struct {
	Have   int    `json:"have"`
	Need   []Hash `json:"need,omitempty"`
	Reason string `json:"reason,omitempty"`
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
	ReasonInvalidLink   = "invalid_link" // a link target that is empty or longer than MaxLinkTarget
	ReasonNoStage       = "no_stage"     // commit: the staged block list is missing, expired, another device's or incomplete
	// Reasons of a refused /stage segment.
	ReasonNoSpace       = "no_space"        // the blocks the file still needs would not fit on the disk
	ReasonTooLarge      = "too_large"       // more than MaxFileBlocks blocks
	ReasonTooManyStages = "too_many_stages" // the device has MaxStagesPerDevice staged uploads already
	ReasonGap           = "gap"             // offset beyond the blocks received; have says where to continue
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
	Link    bool   `json:"link,omitempty"` // a symbolic link: the content is its target (design 6.7)
	// Excluded marks a deletion made because exclusion rules now cover the file (design 8.3). A client keeps its local
	// copy of such a file whatever rules it has at that moment, so a client that has not applied the new rules yet
	// (an offline one, or a pull under way) cannot delete it. Only deletions carry it.
	Excluded bool `json:"excluded,omitempty"`
	// Device is the name of the device that wrote the entry. Only the Current of a conflict result carries it (design
	// 6.1): a client that finds the entry to be its own commit, whose local row was not stored, commits on top of it.
	Device string `json:"device,omitempty"`
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

// MaxLinkTarget is the longest symbolic link target in bytes that is synced (design 6.7).
const MaxLinkTarget = 4096

// LinkNotCreated starts the message of the skipped notice for a link this machine cannot create (design 6.7); the
// Web UI folds these notices to one line per device.
const LinkNotCreated = "cannot create symbolic link"

// Limits of GET /changes pages and POST /blocks/get requests.
const (
	MaxChangesPage  = 5000
	MaxChangeBlocks = 20000 // block hashes per page; a page ends early beyond this
	// MaxGetBlocks bounds the hashes of one /blocks/get request (about 67 bytes of JSON each); its response is
	// bounded by MaxBlockBody bytes, so a request of many small files and one of a few large blocks both fit.
	MaxGetBlocks = 20000
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
