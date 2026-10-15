// Package assetvoip describes reviewed links between source-owned SIP targets
// and physical Assets. It neither probes SIP nor infers identity from addresses.
package assetvoip

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/locations"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
)

var ErrInvalid = errors.New("invalid Asset VoIP association or filter")
var ErrConflict = errors.New("Asset VoIP ownership, endpoint or revision conflict")

type Input struct {
	BindingID             string `json:"binding_id"`
	AgentID               string `json:"agent_id"`
	TargetID              string `json:"target_id"`
	Address               string `json:"address"`
	Transport             string `json:"transport"`
	Source                string `json:"source"`
	Scope                 string `json:"scope"`
	ServiceKind           string `json:"service_kind"`
	Name                  string `json:"name"`
	ExpectedAssetRevision int64  `json:"expected_asset_revision"`
	ExpectedRevision      int64  `json:"expected_revision"`
	Enabled               bool   `json:"enabled"`
	Confirmed             bool   `json:"confirmed"`
	Reason                string `json:"reason"`
}

type Association struct {
	ID                        string    `json:"id"`
	AssetID                   string    `json:"asset_id"`
	BindingID                 string    `json:"binding_id"`
	SourceInstanceID          string    `json:"source_instance_id"`
	InventorySourceInstanceID string    `json:"inventory_source_instance_id"`
	AgentID                   string    `json:"agent_id"`
	TargetID                  string    `json:"target_id"`
	Address                   string    `json:"address"`
	Transport                 string    `json:"transport"`
	Source                    string    `json:"source"`
	Scope                     string    `json:"scope"`
	ServiceKind               string    `json:"service_kind"`
	Name                      string    `json:"name"`
	Enabled                   bool      `json:"enabled"`
	Revision                  int64     `json:"revision"`
	ReviewedSampleID          int64     `json:"reviewed_sample_id"`
	ReviewedAt                time.Time `json:"reviewed_at"`
	ReviewedBy                string    `json:"reviewed_by"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

type Service struct {
	Association
	MappingStatus      string               `json:"mapping_status"`
	MappingReason      string               `json:"mapping_reason"`
	ObservedEndpoint   *Endpoint            `json:"observed_endpoint"`
	Latest             *telephony.Sample    `json:"latest"`
	ClockIssue         *telephony.Sample    `json:"clock_issue"`
	Health             telephony.Health     `json:"health"`
	Monitoring         telephony.Monitoring `json:"monitoring"`
	Incidents          []health.Incident    `json:"incidents"`
	IncidentsTruncated bool                 `json:"incidents_truncated"`
}

type Endpoint struct {
	Address    string    `json:"address"`
	Transport  string    `json:"transport"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
}

type Card struct {
	AssetID        string              `json:"asset_id"`
	AssetName      string              `json:"asset_name"`
	Scope          string              `json:"scope"`
	Placement      locations.Placement `json:"placement"`
	TopologyNodeID string              `json:"topology_node_id"`
	GeneratedAt    time.Time           `json:"generated_at"`
	Items          []Service           `json:"items"`
	Limit          int                 `json:"limit"`
	Offset         int                 `json:"offset"`
}

type Event struct {
	ID            int64        `json:"id"`
	AssociationID string       `json:"association_id"`
	ActorID       string       `json:"actor_id"`
	Before        *Association `json:"before"`
	After         Association  `json:"after"`
	Reason        string       `json:"reason"`
	CreatedAt     time.Time    `json:"created_at"`
}

type HistoryFilter struct {
	From, To *time.Time
	BeforeID int64
	Limit    int
}

// History is scoped to the currently reviewed endpoint identity. After an
// endpoint revision, previous identities remain available through legacy SIP
// history and immutable mapping events, not attributed to the new endpoint.
type HistoryPage struct {
	Association  Association        `json:"association"`
	Items        []telephony.Sample `json:"items"`
	NextBeforeID *int64             `json:"next_before_id"`
	Limit        int                `json:"limit"`
}

func text(v string, min, max int) bool {
	if !utf8.ValidString(v) || len(v) < min || len(v) > max || v != strings.TrimSpace(v) {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func ValidInput(in Input) bool {
	if !text(in.BindingID, 1, 128) || !text(in.AgentID, 1, 128) || !text(in.TargetID, 1, 1024) || !text(in.Address, 1, 1024) || !text(in.Source, 1, 1024) || !text(in.Scope, 0, 128) || !text(in.Name, 1, 128) || !text(in.Reason, 3, 1000) || !in.Confirmed || in.ExpectedAssetRevision < 1 || in.ExpectedRevision < 0 {
		return false
	}
	// Modern Collector metadata can mark sip.options as simulated. The Store
	// verifies the archived observation's actual provenance and session.
	if in.Source == "simulated" && in.Scope == "" {
		return false
	}
	if in.Transport != "udp" && in.Transport != "tcp" && in.Transport != "tls" {
		return false
	}
	return in.ServiceKind == "pbx" || in.ServiceKind == "voice_gateway" || in.ServiceKind == "sip_service"
}

func ValidPage(limit, offset int) bool {
	return limit >= 1 && limit <= 100 && offset >= 0 && offset <= 100000
}

func ValidHistory(in HistoryFilter) bool {
	if in.Limit < 1 || in.Limit > 200 || in.BeforeID < 0 || in.From != nil && in.From.IsZero() || in.To != nil && in.To.IsZero() {
		return false
	}
	if in.From != nil && in.To != nil {
		return in.To.After(*in.From) && in.To.Sub(*in.From) <= 30*24*time.Hour
	}
	return true
}
