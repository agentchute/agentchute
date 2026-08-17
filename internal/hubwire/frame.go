package hubwire

import (
	"errors"
	"fmt"
	"time"

	"github.com/agentchute/agentchute/internal/op"
)

const (
	Protocol       = "agentchute-hub"
	Version        = 1
	MinVersion     = 1
	MaxControlLine = 64 << 10
	MaxBody        = 4 << 20
	MaxStatusRows  = 64
)

type RequestBase struct {
	T  string `json:"t"`
	ID int64  `json:"id"`
}

type ResponseBase struct {
	T  string `json:"t"`
	Re int64  `json:"re,omitempty"`
}

type Hello struct {
	RequestBase
	Proto string `json:"proto"`
	V     int    `json:"v"`
	MinV  int    `json:"min_v"`
	Agent string `json:"agent"`
	Bin   string `json:"bin"`
}

type HelloOK struct {
	ResponseBase
	V        int       `json:"v"`
	Agent    string    `json:"agent"`
	Pool     string    `json:"pool"`
	Pool12   string    `json:"pool12"`
	Writable bool      `json:"writable"`
	HubBin   string    `json:"hub_bin"`
	HubTime  time.Time `json:"hub_time"`
}

type Send struct {
	RequestBase
	To         string `json:"to"`
	Ask        bool   `json:"ask,omitempty"`
	ReplyByS   int64  `json:"reply_by_s,omitempty"`
	ServeToken string `json:"serve_token,omitempty"`
}

type SendOK struct {
	ResponseBase
	Filename       string `json:"filename"`
	Ref            string `json:"ref"`
	Committed      bool   `json:"committed"`
	DurabilityNote string `json:"durability_note"`
	OwedNote       string `json:"owed_note"`
}

type Message struct {
	ResponseBase
	Filename      string `json:"filename"`
	Sender        string `json:"sender"`
	Stamp         string `json:"stamp"`
	Redelivered   bool   `json:"redelivered,omitempty"`
	ReplyRequired bool   `json:"reply_required,omitempty"`
	ReplyRef      string `json:"reply_ref,omitempty"`
}

type OwedItem struct {
	ResponseBase
	To         string    `json:"to"`
	From       string    `json:"from"`
	Seq        uint64    `json:"seq,omitempty"`
	Stamp      string    `json:"stamp,omitempty"`
	Suffix     string    `json:"suffix,omitempty"`
	By         time.Time `json:"by"`
	RecordedAt time.Time `json:"recorded_at"`
	Ref        string    `json:"ref"`
}

type Check struct {
	RequestBase
	Limit     int  `json:"limit,omitempty"`
	NoArchive bool `json:"no_archive,omitempty"`
}

type CheckOK struct {
	ResponseBase
	Claimed     int `json:"claimed"`
	Redelivered int `json:"redelivered"`
	Quarantined int `json:"quarantined"`
	OwedExpired int `json:"owed_expired"`
}

type Ack struct{ RequestBase }

type AckItem struct {
	ResponseBase
	Filename    string `json:"filename"`
	ArchivePath string `json:"archive_path"`
}

type AckOK struct {
	ResponseBase
	Acked        int      `json:"acked"`
	GateClear    bool     `json:"gate_clear"`
	BlockReasons []string `json:"block_reasons,omitempty"`
}

type Register struct {
	RequestBase
	Vendor       *string  `json:"vendor,omitempty"`
	Host         string   `json:"host"`
	Bio          *string  `json:"bio,omitempty"`
	WorkingRepos []string `json:"working_repos,omitempty"`
	Announce     bool     `json:"announce,omitempty"`
	Sweep        bool     `json:"sweep,omitempty"`
	ServeToken   string   `json:"serve_token,omitempty"`
}

type Registration struct {
	AgentID         string    `json:"agent_id"`
	ProtocolVersion int       `json:"v"`
	Vendor          string    `json:"vendor"`
	ControlRepo     string    `json:"control_repo"`
	WorkingRepos    []string  `json:"working_repos,omitempty"`
	Host            string    `json:"host"`
	LastSeen        time.Time `json:"last_seen"`
}

type Announce struct {
	Sent     int      `json:"sent"`
	Total    int      `json:"total"`
	Warnings []string `json:"warnings"`
}

type RegisterOK struct {
	ResponseBase
	Announce      *Announce    `json:"announce"`
	Pending       int          `json:"pending"`
	Reg           Registration `json:"reg"`
	InboxDir      string       `json:"inbox_dir"`
	Refreshed     bool         `json:"refreshed"`
	ExistingFound bool         `json:"existing_found"`
	ResolvedHost  string       `json:"resolved_host"`
	Warnings      []string     `json:"warnings"`
}

type Status struct{ RequestBase }

type StatusOK struct {
	ResponseBase
	Agents []op.StatusAgent `json:"agents"`
	// Truncated must remain present when false: EncodeStatus measures candidates
	// with false, whose encoding is one byte longer than true. Omitting false
	// would make the wire budget optimistic at the 64 KiB boundary.
	Truncated bool      `json:"truncated"`
	Now       time.Time `json:"now"`
}

type Gate struct {
	RequestBase
	Phase          string `json:"phase"`
	RequireConfirm bool   `json:"require_confirm,omitempty"`
	AckStaleReg    bool   `json:"ack_stale_reg,omitempty"`
}

type GateOK struct {
	ResponseBase
	op.GateResp
}

type Pending struct {
	RequestBase
	ShowBody bool `json:"show_body,omitempty"`
}

type PendingOK struct {
	ResponseBase
	Unread    int  `json:"unread"`
	Owed      int  `json:"owed"`
	Malformed int  `json:"malformed"`
	NeedsBoot bool `json:"needs_boot,omitempty"`
}

type CleanOwed struct {
	RequestBase
	Apply bool `json:"apply,omitempty"`
}

type CleanOwedOK struct {
	ResponseBase
	Agent   string   `json:"agent"`
	Pruned  []string `json:"pruned"`
	Applied bool     `json:"applied"`
}

type LeaseAcquire struct{ RequestBase }

type LeaseOK struct {
	ResponseBase
	Token string `json:"token"`
}

type Tick struct{ RequestBase }

type TickOK struct {
	ResponseBase
	Pending  int      `json:"pending"`
	Skipped  int      `json:"skipped"`
	Swept    []string `json:"swept,omitempty"`
	Warnings []string `json:"warnings"`
}

type LeaseRelease struct{ RequestBase }
type ReleaseOK struct{ ResponseBase }

type Note struct {
	ResponseBase
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

type Error struct {
	ResponseBase
	Code        string `json:"code"`
	Msg         string `json:"msg"`
	Retriable   bool   `json:"retriable"`
	ClaimedHeld bool   `json:"claimed_held,omitempty"`
}

func NewError(re int64, err error) Error {
	f := Error{
		ResponseBase: ResponseBase{T: "error", Re: re},
		Code:         CodeFor(err),
		Msg:          err.Error(),
	}
	var pe *ProtocolError
	if errors.As(err, &pe) {
		f.Retriable = pe.Retriable
		f.ClaimedHeld = pe.ClaimedHeld
	}
	return f
}

func ValidateNoteLevel(level string) error {
	if level != op.NoteWarn && level != op.NoteInfo {
		return protocolError(CodeMalformedFrame, fmt.Sprintf("invalid note level %q", level))
	}
	return nil
}
