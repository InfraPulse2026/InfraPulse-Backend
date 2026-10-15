package analysisai

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/correlation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"math"
	"strconv"
)

type EvidenceReference struct {
	Ref                   string   `json:"ref"`
	IncidentID            string   `json:"incident_id,omitempty"`
	SignalID              string   `json:"signal_id,omitempty"`
	AssetIDs              []string `json:"asset_ids,omitempty"`
	CorrelationEvidenceID int64    `json:"correlation_evidence_id,omitempty"`
}

// References remain local in Preview. Only the pseudonymized Payload is sent.
func ExportCorrelated(c correlation.Case, seed string, current map[string]health.Incident) (Payload, []EvidenceReference, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return Payload{}, nil, err
	}
	defer clear(key)
	alias := func(kind, value string) string {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(kind + ":" + value))
		return kind + "_" + hex.EncodeToString(h.Sum(nil)[:8])
	}
	root, ok := current[seed]
	if !ok || !rules[root.RuleID] {
		return Payload{}, nil, ErrInvalid
	}
	payload := Payload{Version: PromptVersion, Facts: []Fact{}, Relations: []Relation{}, LogSeverityCounts: map[string]int64{}, LogEventCounts: map[string]int64{}, Limitations: []string{"Correlation is context, not proof of cause.", "All source labels, names, addresses, raw logs/configuration and credentials are excluded.", "Timestamps are relative; uncertain time does not establish event order.", "Reported events and operator review do not establish physical recovery.", "Physical adjacency does not establish routing or redundancy.", "No automatic action is authorized. Context is frozen for this preview; current state may change."}}
	references := []EvidenceReference{}
	selected := map[string]Fact{}
	// Always include the requested seed, even if it is not among the first members.
	members := append([]correlation.Membership{}, c.Signals...)
	for i := range members {
		if members[i].Signal.IncidentID == seed {
			members[0], members[i] = members[i], members[0]
			break
		}
	}
	for _, member := range members {
		v, ok := current[member.Signal.IncidentID]
		if !ok || !rules[v.RuleID] || v.Scope != root.Scope || (root.Scope != "" && v.AgentID != root.AgentID) {
			continue
		}
		if len(payload.Facts) >= 16 {
			payload.Limitations = append(payload.Limitations, "Signal export capped at sixteen.")
			break
		}
		status := oneOf(v.Signal.Status, "ok", "pending", "warning", "critical", "unknown", "stale")
		if v.EvidenceStatus != "fresh" {
			status = "stale"
		}
		source := oneOf(member.Signal.SourceKind, "zabbix", "prometheus", "ansible")
		if source == "unknown" && v.AgentID != "" {
			source = "collector"
		}
		sourceID := member.Signal.SourceKind
		if member.Signal.SourceInstanceID != nil {
			sourceID = *member.Signal.SourceInstanceID
		} else if v.AgentID != "" {
			sourceID = v.AgentID
		}
		identity, _ := json.Marshal([]string{v.AgentID, v.DeviceID, v.ResourceKind, v.ResourceID, v.ResourceContext, v.Scope})
		offset := v.LastObservedAt.Sub(root.OpenedAt).Seconds()
		quality := oneOf(member.Signal.TimeQuality, "exact", "estimated", "received_only", "skewed")
		usable := quality == "exact" || quality == "estimated"
		if math.Abs(offset) > 31*24*3600 {
			offset = 0
			usable = false
		}
		fact := Fact{Ref: "fact_" + strconv.Itoa(len(payload.Facts)+1), Incident: alias("incident", v.ID), Resource: alias("resource", string(identity)), Rule: v.RuleID, Status: status, Severity: oneOf(v.Severity, "warning", "critical"), Simulated: v.Scope != "" || v.Source == "simulated", SecondsFromSeed: offset, SourceKind: source, Source: alias("source", source+":"+sourceID), Category: oneOf(member.Signal.Category, "availability", "performance", "network", "telephony", "collector", "external", "hardware", "configuration", "topology", "log"), State: oneOf(v.State, "open", "acknowledged", "resolved"), TimeQuality: quality, TemporalUsable: usable, Assets: []string{}, Unit: oneOf(v.Signal.Unit, "percent", "per_second", "milliseconds", "boolean", "state_grade", "changes_in_300_seconds"), Value: finite(v.Signal.NumericValue), SampleCount: len(v.Signal.Evidence) + len(v.Signal.ResourceEvidence), WindowSeconds: v.Signal.WindowSeconds, ObservationKind: "measured_condition"}
		if fact.Unit == "unknown" && v.Signal.Value != nil {
			fact.Unit = "percent"
			fact.Value = finite(v.Signal.Value)
		}
		if v.ResourceKind == "log" || v.ResourceKind == "topology" {
			fact.ObservationKind = "reported_event"
		}
		if v.ResourceKind == "external" {
			fact.ObservationKind = "external_alert"
		}
		if v.Signal.Log != nil {
			fact.EventCount = v.Signal.Log.EventCount
		}
		if v.Signal.Topology != nil {
			fact.EventCount = v.Signal.Topology.EventCount
		}
		if v.Signal.Configuration != nil {
			fact.ObservationKind = "configuration_comparison"
			fact.ChangeCount = v.Signal.Configuration.TotalChanges
		}
		if v.RuleID == "cpu.trend" {
			fact.Slope = finite(v.Signal.Slope)
			fact.Fit = finite(v.Signal.Fit)
			if status != "stale" {
				fact.SecondsToCritical = finite(v.Signal.SecondsToCritical)
			}
		}
		if v.Signal.Collector != nil {
			connected := v.Signal.Collector.Connected
			fact.Connected = &connected
		}
		for _, asset := range member.Signal.AssetIDs {
			if len(fact.Assets) >= 16 {
				break
			}
			fact.Assets = append(fact.Assets, alias("asset", asset))
		}
		payload.Facts = append(payload.Facts, fact)
		selected[member.Signal.ID] = fact
		references = append(references, EvidenceReference{Ref: fact.Ref, IncidentID: v.ID, SignalID: member.Signal.ID, AssetIDs: append([]string{}, member.Signal.AssetIDs...)})
	}
	seen := map[string]bool{}
	for _, e := range c.Evidence {
		left, lok := selected[e.LeftSignalID]
		right, rok := selected[e.RightSignalID]
		kind := oneOf(e.Kind, "same_asset", "same_service", "topology_neighbor", "configuration_precedes", "recurring_pattern", "temporal_proximity", "same_site", "source_diversity")
		if !lok || !rok || kind == "unknown" {
			continue
		}
		if kind == "configuration_precedes" && (left.TimeQuality != "exact" || right.TimeQuality != "exact" || !left.TemporalUsable || !right.TemporalUsable) {
			continue
		}
		if kind == "temporal_proximity" && (!left.TemporalUsable || !right.TemporalUsable) {
			continue
		}
		id := e.LeftSignalID + "|" + e.RightSignalID + "|" + kind
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(payload.Relations) >= 32 {
			payload.Limitations = append(payload.Limitations, "Relation export capped at thirty-two.")
			break
		}
		ref := "relation_" + strconv.Itoa(len(payload.Relations)+1)
		payload.Relations = append(payload.Relations, Relation{Ref: ref, From: left.Incident, To: right.Incident, Kind: kind, Strength: "context", FromFact: left.Ref, ToFact: right.Ref})
		references = append(references, EvidenceReference{Ref: ref, CorrelationEvidenceID: e.ID})
	}
	if c.Truncated {
		payload.Limitations = append(payload.Limitations, "Stored correlation detail was truncated.")
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(payload.Facts) == 0 || len(raw) > 32<<10 {
		return Payload{}, nil, ErrInvalid
	}
	return payload, references, nil
}
