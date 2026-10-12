package logbook

import (
	"encoding/json"
	"time"
)

type Audit struct {
	EventKey   string  `json:"event_key"`
	EventID    string  `json:"event_id"`
	RecordType string  `json:"record_type"`
	UID        *uint32 `json:"uid"`
	AUID       *uint32 `json:"auid"`
	PID        *uint32 `json:"pid"`
	Executable string  `json:"executable"`
	Syscall    string  `json:"syscall"`
	Success    *bool   `json:"success"`
	Result     string  `json:"result"`
	Host       string  `json:"host"`
}

type Record struct {
	ID                int64           `json:"id"`
	AgentID           string          `json:"agent_id"`
	DeviceID          string          `json:"device_id"`
	RecordID          string          `json:"record_id"`
	MessageID         string          `json:"message_id"`
	Source            string          `json:"source"`
	SourceID          string          `json:"source_id"`
	EventAt           time.Time       `json:"event_at"`
	ObservedAt        time.Time       `json:"observed_at"`
	OriginalTimestamp *time.Time      `json:"original_timestamp"`
	ReceivedAt        time.Time       `json:"received_at"`
	TimeBasis         string          `json:"time_basis"`
	Severity          *uint32         `json:"severity"`
	SeverityName      string          `json:"severity_name"`
	Facility          *uint32         `json:"facility"`
	Host              string          `json:"host"`
	Process           string          `json:"process"`
	ProcessID         string          `json:"process_id"`
	Message           string          `json:"message"`
	Raw               string          `json:"raw"`
	Truncated         bool            `json:"truncated"`
	Redacted          bool            `json:"redacted"`
	FileIdentity      string          `json:"file_identity"`
	Offset            int64           `json:"offset"`
	Audit             *Audit          `json:"audit"`
	Syslog            json.RawMessage `json:"syslog,omitempty"`
	Observation       json.RawMessage `json:"observation,omitempty"`
	RawPayloadOmitted bool            `json:"raw_payload_omitted,omitempty"`
}

type Filter struct {
	AgentID, DeviceID, Source, SourceID, Host, Process, RecordType, EventID, EventKey, Executable string
	Q, Contains                                                                                   string
	Severity, Facility, UID, AUID, PID                                                            *uint32
	SeverityUnknown                                                                               bool
	Success                                                                                       *bool
	SuccessUnknown                                                                                bool
	AuditOnly                                                                                     bool
	From, To                                                                                      *time.Time
	BeforeID                                                                                      int64
	Limit                                                                                         int
}

type Page struct {
	Items        []Record `json:"items"`
	NextBeforeID *int64   `json:"next_before_id"`
	Limit        int      `json:"limit"`
}
type Summary struct {
	GeneratedAt   time.Time        `json:"generated_at"`
	Total         int64            `json:"total"`
	AuditRecords  int64            `json:"audit_records"`
	AuditFailures int64            `json:"audit_failures"`
	Redacted      int64            `json:"redacted"`
	Truncated     int64            `json:"truncated"`
	Severity      map[string]int64 `json:"severity"`
}

type AuditEvent struct {
	CursorID        int64     `json:"cursor_id"`
	Key             string    `json:"key"`
	AgentID         string    `json:"agent_id"`
	DeviceID        string    `json:"device_id"`
	Source          string    `json:"source"`
	SourceID        string    `json:"source_id"`
	FileIdentity    string    `json:"file_identity"`
	Host            string    `json:"host"`
	EventID         string    `json:"event_id"`
	FirstEventAt    time.Time `json:"first_event_at"`
	LastEventAt     time.Time `json:"last_event_at"`
	FirstReceivedAt time.Time `json:"first_received_at"`
	LastReceivedAt  time.Time `json:"last_received_at"`
	RecordCount     int64     `json:"record_count"`
	RecordTypes     []string  `json:"record_types"`
	Outcome         string    `json:"outcome"`
	Completeness    string    `json:"completeness"`
}

func SeverityName(v *uint32) string {
	if v == nil || *v > 7 {
		return "unknown"
	}
	return [...]string{"emergency", "alert", "critical", "error", "warning", "notice", "info", "debug"}[*v]
}
