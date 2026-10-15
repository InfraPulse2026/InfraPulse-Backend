// Package automation implements a closed, read-only Ansible job protocol.
package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/crypto/ssh"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid automation request")
var ErrConflict = errors.New("automation revision or idempotency conflict")
var ErrUnavailable = errors.New("automation worker unavailable")

const MaxConfiguration = 4 << 20
const MaxFacts = 128 << 10

type Template struct {
	ID         string `json:"id"`
	Platform   string `json:"platform"`
	Collection string `json:"collection"`
	ReadOnly   bool   `json:"read_only"`
}

// These templates are reviewed server code, never supplied by API clients.
func Templates() []Template {
	return []Template{
		{"cisco_ios_read_config_v1", "cisco.ios.ios", "cisco.ios", true},
		{"arista_eos_read_config_v1", "arista.eos.eos", "arista.eos", true},
		{"vyos_read_config_v1", "vyos.vyos.vyos", "vyos.vyos", true},
	}
}
func Lookup(id string) (Template, bool) {
	for _, t := range Templates() {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

var refPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func Identifier(s string) bool { return refPattern.MatchString(s) }

type TargetInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	AssetRevision    int64  `json:"asset_revision"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	TemplateID       string `json:"template_id"`
	CredentialRef    string `json:"credential_ref"`
	SSHHostKey       string `json:"ssh_host_key"`
	Enabled          bool   `json:"enabled"`
	Reason           string `json:"reason"`
}
type Target struct {
	TargetInput
	AssetID   string    `json:"asset_id"`
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
	// Operation is populated only by the reviewed remediation workflow. It is
	// never accepted by the generic automation API.
	Operation *Operation `json:"operation,omitempty"`
}

type Operation struct {
	Type          string `json:"type"`
	RemediationID string `json:"remediation_id"`
	Interface     string `json:"interface"`
	VLAN          int    `json:"vlan"`
}

var safeAccessInterface = regexp.MustCompile(`^(?:GigabitEthernet|TenGigabitEthernet|TwentyFiveGigE|FortyGigabitEthernet|HundredGigE|FastEthernet|Ethernet)[0-9]+(?:/[0-9]+){0,3}$`)

func (v Operation) Validate() error {
	if v.Type != "restore_access_vlan" || !Identifier(v.RemediationID) || !safeAccessInterface.MatchString(v.Interface) || v.VLAN < 1 || v.VLAN > 4094 {
		return ErrInvalid
	}
	return nil
}

func (v TargetInput) Validate() error {
	addr, err := netip.ParseAddr(v.Host)
	_, ok := Lookup(v.TemplateID)
	key, _, _, rest, keyErr := ssh.ParseAuthorizedKey([]byte(v.SSHHostKey))
	if err != nil || addr.Is4In6() || addr.IsUnspecified() || addr.IsMulticast() || v.Host != addr.String() || v.Port < 1 || v.Port > 65535 || !ok || !Identifier(v.CredentialRef) || v.ExpectedRevision < 0 || v.AssetRevision < 1 || keyErr != nil || len(rest) != 0 || key == nil || len(v.SSHHostKey) > 4096 || !Reason(v.Reason) {
		return ErrInvalid
	}
	if v.SSHHostKey != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) {
		return ErrInvalid
	}
	return nil
}
func Reason(s string) bool {
	return len(strings.TrimSpace(s)) >= 3 && len(s) <= 512 && !strings.ContainsAny(s, "\x00\r\n")
}

type Input struct {
	TargetRevision int64  `json:"target_revision"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Reason         string `json:"reason"`
}

func (v Input) Validate() error {
	if v.TargetRevision < 1 || v.TimeoutSeconds < 30 || v.TimeoutSeconds > 600 || !Reason(v.Reason) {
		return ErrInvalid
	}
	return nil
}

type Job struct {
	ID             string     `json:"id"`
	AssetID        string     `json:"asset_id"`
	TargetRevision int64      `json:"target_revision"`
	TemplateID     string     `json:"template_id"`
	ActorID        string     `json:"actor_id"`
	State          string     `json:"state"`
	ErrorCode      string     `json:"error_code"`
	TimeoutSeconds int        `json:"timeout_seconds"`
	Reason         string     `json:"reason"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	SnapshotID     string     `json:"snapshot_id"`
	// Execution-only values must never appear in REST or logs.
	Target     Target `json:"-"`
	ClaimToken string `json:"-"`
}
type Event struct {
	ID    int64     `json:"id"`
	JobID string    `json:"job_id"`
	Code  string    `json:"code"`
	At    time.Time `json:"at"`
}
type Snapshot struct {
	ID         string    `json:"id"`
	AssetID    string    `json:"asset_id"`
	JobID      string    `json:"job_id"`
	TemplateID string    `json:"template_id"`
	Digest     string    `json:"sha256"`
	Bytes      int       `json:"bytes"`
	ObservedAt time.Time `json:"observed_at"`
}
type Result struct {
	Configuration string          `json:"configuration"`
	Facts         json.RawMessage `json:"facts"`
	ErrorCode     string          `json:"error_code,omitempty"`
}

func (v Result) Validate() error {
	if v.ErrorCode != "" {
		return ErrInvalid
	}
	if len(strings.TrimSpace(v.Configuration)) == 0 || len(v.Configuration) > MaxConfiguration || strings.ContainsRune(v.Configuration, 0) || len(v.Facts) > MaxFacts || !json.Valid(v.Facts) || !strings.HasPrefix(strings.TrimSpace(string(v.Facts)), "{") {
		return ErrInvalid
	}
	return nil
}
func Digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

type WorkerStatus struct {
	ID        string            `json:"id"`
	Available bool              `json:"available"`
	Versions  map[string]string `json:"versions"`
	Templates []string          `json:"templates"`
	LastSeen  time.Time         `json:"last_seen"`
	ErrorCode string            `json:"error_code"`
}
type Repository interface {
	AutomationWorker(context.Context, WorkerStatus) error
	ClaimAutomation(context.Context, string) (Job, error)
	AutomationRunning(context.Context, Job) (bool, error)
	FinishAutomation(context.Context, Job, string, Result) error
}
type Executor interface {
	Probe(context.Context) WorkerStatus
	Execute(context.Context, Job) Result
}
