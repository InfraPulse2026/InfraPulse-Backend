// Package diagnosis relates observed symptoms; it never proves a root cause,
// infers an unconfigured service path, or dispatches a device command.
package diagnosis

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
)

var ErrInvalid = errors.New("invalid diagnostic context limits")

type Link struct {
	EdgeID          string    `json:"edge_id"`
	AgentID         string    `json:"agent_id"`
	FromDevice      string    `json:"from_device_id"`
	ToDevice        string    `json:"to_device_id"`
	LocalInterface  string    `json:"local_interface_id"`
	RemoteInterface string    `json:"remote_interface_id"`
	Source          string    `json:"source"`
	Scope           string    `json:"scope"`
	Protocol        string    `json:"protocol"`
	Confidence      string    `json:"resolution_confidence"`
	ObservedAt      time.Time `json:"observed_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}
type Related struct {
	Incident    health.Incident `json:"incident"`
	Relation    string          `json:"relation"`
	Strength    string          `json:"strength"`
	Basis       []string        `json:"basis"`
	Links       []Link          `json:"topology_links"`
	CausalClaim bool            `json:"causal_claim"`
}
type Context struct {
	Version            string          `json:"version"`
	GeneratedAt        time.Time       `json:"generated_at"`
	Seed               health.Incident `json:"incident"`
	WindowSeconds      int             `json:"window_seconds"`
	Related            []Related       `json:"related"`
	Truncated          bool            `json:"truncated"`
	TopologyConsidered bool            `json:"topology_considered"`
	Limitations        []string        `json:"limitations"`
}

func simulated(v health.Incident) bool { return v.Source == "simulated" || v.Scope != "" }
func compatible(a, b health.Incident) bool {
	return a.AgentID == b.AgentID && simulated(a) == simulated(b) && (!simulated(a) || a.Scope == b.Scope)
}
func temporal(a, b health.Incident, now time.Time, window time.Duration) bool {
	ae, be := now, now
	if a.ResolvedAt != nil {
		ae = *a.ResolvedAt
	}
	if b.ResolvedAt != nil {
		be = *b.ResolvedAt
	}
	return !a.OpenedAt.After(be.Add(window)) && !b.OpenedAt.After(ae.Add(window))
}
func activeProblem(v health.Incident) bool {
	return v.State != "resolved" && v.EvidenceStatus == "fresh" && (v.Signal.Status == "warning" || v.Signal.Status == "critical")
}
func Build(now time.Time, seed health.Incident, candidates []health.Incident, links []Link, window, limit int, truncated bool) Context {
	result := Context{Version: "1", GeneratedAt: now, Seed: seed, WindowSeconds: window, Related: []Related{}, Truncated: truncated, Limitations: []string{
		"relations are contextual hypotheses, not a proven root cause",
		"shared collector/time does not establish a physical or service dependency",
		"SIP target-to-device routing/service dependencies are not configured in this module",
		"topology relations use fresh current discovery only; historical topology is not reconstructed",
		"expired/ambiguous/weak-resolution links and mismatched simulation sessions are excluded",
		"stale/missing observations and collector loss do not prove downstream device failure",
		"no device command or external AI request is performed",
	}}
	result.TopologyConsidered = activeProblem(seed) && seed.DeviceID != ""
	for _, candidate := range candidates {
		if candidate.ID == seed.ID || !compatible(seed, candidate) || !temporal(seed, candidate, now, time.Duration(window)*time.Second) {
			continue
		}
		item := Related{Incident: candidate, Relation: "same_collector_temporal_context", Strength: "weak", Basis: []string{"same_reporting_agent", "episode_overlap_or_nearby_time"}, Links: []Link{}}
		if seed.ResourceKind == "agent" || candidate.ResourceKind == "agent" {
			item.Relation = "collector_observability_context"
			item.Strength = "moderate"
			item.Basis = append(item.Basis, "collector_connection_is_part_of_the_observation_path")
		} else if seed.DeviceID != "" && seed.DeviceID == candidate.DeviceID {
			item.Relation = "same_device_context"
			item.Strength = "moderate"
			item.Basis = append(item.Basis, "same_agent_scoped_device_id")
			if seed.ResourceKind == candidate.ResourceKind && seed.ResourceID == candidate.ResourceID && seed.ResourceContext == candidate.ResourceContext && seed.Source == candidate.Source {
				item.Relation = "same_resource_context"
				item.Strength = "strong_context"
				item.Basis = append(item.Basis, "same_resource_and_source")
			}
		}
		if activeProblem(seed) && activeProblem(candidate) && seed.DeviceID != "" && candidate.DeviceID != "" && seed.DeviceID != candidate.DeviceID {
			for _, link := range links {
				if link.AgentID != seed.AgentID || link.Confidence != "high" || link.ObservedAt.After(now) || !link.ExpiresAt.After(now) || (link.Source == "simulated") != simulated(seed) || simulated(seed) && link.Scope != seed.Scope {
					continue
				}
				direct := link.FromDevice == seed.DeviceID && link.ToDevice == candidate.DeviceID
				reverse := link.FromDevice == candidate.DeviceID && link.ToDevice == seed.DeviceID
				if !direct && !reverse {
					continue
				}
				item.Relation = "fresh_discovery_adjacency"
				item.Strength = "moderate"
				if len(item.Links) >= 10 {
					result.Truncated = true
					continue
				}
				item.Links = append(item.Links, link)
				matchingPort := seed.ResourceKind == "interface" && ((direct && link.LocalInterface == seed.ResourceID) || (reverse && link.RemoteInterface != "" && link.RemoteInterface == seed.ResourceID))
				protocol := strings.ToLower(link.Protocol)
				directDiscovery := protocol == "lldp" || protocol == "cdp" || protocol == "discovery_protocol_lldp" || protocol == "discovery_protocol_cdp"
				if matchingPort && directDiscovery && (seed.RuleID == "interface.oper_down" || seed.RuleID == "interface.flap") {
					item.Relation = "port_and_neighbor_symptom_candidate"
					item.Strength = "strong_context"
				}
			}
			if len(item.Links) > 0 {
				item.Basis = append(item.Basis, "fresh_agent_discovery", "unambiguous_high_confidence_device_resolution")
			}
		}
		result.Related = append(result.Related, item)
	}
	weight := func(value string) int {
		switch value {
		case "strong_context":
			return 3
		case "moderate":
			return 2
		default:
			return 1
		}
	}
	sort.Slice(result.Related, func(i, j int) bool {
		a, b := result.Related[i], result.Related[j]
		if weight(a.Strength) != weight(b.Strength) {
			return weight(a.Strength) > weight(b.Strength)
		}
		if !a.Incident.OpenedAt.Equal(b.Incident.OpenedAt) {
			return a.Incident.OpenedAt.After(b.Incident.OpenedAt)
		}
		return a.Incident.ID < b.Incident.ID
	})
	if len(result.Related) > limit {
		result.Related = result.Related[:limit]
		result.Truncated = true
	}
	return result
}
