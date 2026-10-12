// Package assetlogs provides an additive, safe Asset view of Collector logs.
// A source-owned device reference associates a record with inventory; it does
// not authenticate the physical syslog sender or its claimed hostname.
package assetlogs

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
)

const MaxLimit = 200
const MaxRawBytes = 256 << 10

var ErrFilter = errors.New("invalid asset log filter")

type Filter struct {
	Log              logbook.Filter
	SourceInstanceID string
	// Nil selects all scopes. An empty value selects real records, while a
	// nonempty value is the exact simulation session, never a wildcard.
	RecordScope *string
}

func ValidateFilter(f Filter) error {
	l := f.Log
	if l.Limit < 1 || l.Limit > MaxLimit || l.BeforeID < 0 || l.Q != "" && l.Contains != "" {
		return ErrFilter
	}
	values := []string{f.SourceInstanceID, l.AgentID, l.DeviceID, l.Source, l.SourceID, l.Host, l.Process, l.RecordType, l.EventID, l.EventKey, l.Executable, l.Q, l.Contains}
	if f.RecordScope != nil {
		values = append(values, *f.RecordScope)
	}
	for _, v := range values {
		if len(v) > 256 || strings.ContainsRune(v, 0) {
			return ErrFilter
		}
	}
	if l.From != nil && l.To != nil && !l.From.Before(*l.To) {
		return ErrFilter
	}
	if l.Severity != nil && *l.Severity > 7 || l.Facility != nil && *l.Facility > 23 || l.Severity != nil && l.SeverityUnknown || l.Success != nil && l.SuccessUnknown {
		return ErrFilter
	}
	return nil
}

func ValidAssetID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Record deliberately has no raw text field. Adding query parameters cannot
// change this type into the privileged raw response.
type Record struct {
	AssetID           string          `json:"asset_id"`
	BindingID         string          `json:"binding_id"`
	SourceInstanceID  string          `json:"source_instance_id"`
	RecordScope       string          `json:"record_scope"`
	Association       string          `json:"association"`
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
	Truncated         bool            `json:"truncated"`
	Redacted          bool            `json:"redacted"`
	FileIdentity      string          `json:"file_identity"`
	Offset            int64           `json:"offset"`
	Audit             *logbook.Audit  `json:"audit"`
	Syslog            json.RawMessage `json:"syslog,omitempty"`
	Observation       json.RawMessage `json:"observation,omitempty"`
	RawPayloadOmitted bool            `json:"raw_payload_omitted,omitempty"`
}

func Public(r logbook.Record, asset, binding, source, scope string) Record {
	return Record{AssetID: asset, BindingID: binding, SourceInstanceID: source, RecordScope: scope, Association: "source_owned_device_reference", ID: r.ID, AgentID: r.AgentID, DeviceID: r.DeviceID, RecordID: r.RecordID, MessageID: r.MessageID, Source: r.Source, SourceID: r.SourceID, EventAt: r.EventAt, ObservedAt: r.ObservedAt, OriginalTimestamp: r.OriginalTimestamp, ReceivedAt: r.ReceivedAt, TimeBasis: r.TimeBasis, Severity: r.Severity, SeverityName: r.SeverityName, Facility: r.Facility, Host: r.Host, Process: r.Process, ProcessID: r.ProcessID, Message: r.Message, Truncated: r.Truncated, Redacted: r.Redacted, FileIdentity: r.FileIdentity, Offset: r.Offset, Audit: r.Audit, Syslog: withoutRaw(r.Syslog), Observation: withoutRaw(r.Observation), RawPayloadOmitted: r.RawPayloadOmitted}
}

// SafeLegacy preserves the established JSON fields while masking privileged
// raw text. It does not mutate the stored record or its raw archive.
func SafeLegacy(r logbook.Record) logbook.Record {
	r.Raw = ""
	r.Syslog = withoutRaw(r.Syslog)
	r.Observation = withoutRaw(r.Observation)
	return r
}

// Normal ingestion already removes raw syslog octets. Defend the Asset public
// projection against old imported JSON or accidentally added opaque raw fields.
func withoutRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if d.Decode(&value) != nil {
		return nil
	}
	var clean func(any)
	clean = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				switch k {
				case "raw", "raw_payload", "rawPayload", "raw_message", "rawMessage":
					delete(x, k)
				default:
					clean(child)
				}
			}
		case []any:
			for _, child := range x {
				clean(child)
			}
		}
	}
	clean(value)
	out, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return out
}

type Page struct {
	AssetID      string   `json:"asset_id"`
	Items        []Record `json:"items"`
	NextBeforeID *int64   `json:"next_before_id"`
	Limit        int      `json:"limit"`
}

type AuditEvent struct {
	logbook.AuditEvent
	AssetID          string `json:"asset_id"`
	BindingID        string `json:"binding_id"`
	SourceInstanceID string `json:"source_instance_id"`
	RecordScope      string `json:"record_scope"`
	Association      string `json:"association"`
}

type AuditPage struct {
	AssetID      string       `json:"asset_id"`
	Items        []AuditEvent `json:"items"`
	NextBeforeID *int64       `json:"next_before_id"`
	Limit        int          `json:"limit"`
}

// Raw exposes only the bounded, sanitized raw text already retained by the
// logbook projection. Archived binary payloads and unsanitized inbox envelopes
// are not exposed by this API, even to administrators.
type Raw struct {
	AssetID                  string `json:"asset_id"`
	BindingID                string `json:"binding_id"`
	SourceInstanceID         string `json:"source_instance_id"`
	RecordScope              string `json:"record_scope"`
	ID                       int64  `json:"id"`
	Raw                      string `json:"raw"`
	Redacted                 bool   `json:"redacted"`
	Truncated                bool   `json:"truncated"`
	RawPayloadAvailable      bool   `json:"raw_payload_available"`
	ArchivePayloadNotExposed bool   `json:"archive_payload_not_exposed"`
}
