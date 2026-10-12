package topology

import (
	"encoding/base64"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/availability"
)

type Device struct {
	Availability                                                                           availability.State
	StateReported                                                                          bool
	AgentID, ID, Name, Hostname, ChassisID, ManagementAddress, Vendor, Model, Type, Source string
	ReportedOnline, AgentConnected                                                         bool
	LastSeen                                                                               time.Time
}

type Node struct {
	Availability         *availability.State `json:"availability,omitempty"`
	ID                   string              `json:"id"`
	AgentID              string              `json:"agent_id"`
	DeviceID             string              `json:"device_id,omitempty"`
	Known                bool                `json:"known"`
	Label                string              `json:"label"`
	Hostname             string              `json:"hostname,omitempty"`
	ManagementAddress    string              `json:"management_address,omitempty"`
	Vendor               string              `json:"vendor,omitempty"`
	Model                string              `json:"model,omitempty"`
	Type                 string              `json:"type,omitempty"`
	Source               string              `json:"source"`
	ReportedOnline       *bool               `json:"reported_online,omitempty"`
	AgentConnected       *bool               `json:"agent_connected,omitempty"`
	LastSeen             *time.Time          `json:"last_seen,omitempty"`
	RemoteChassisSubtype uint32              `json:"remote_chassis_subtype,omitempty"`
	RemoteChassisID      string              `json:"remote_chassis_id,omitempty"`
}

type Resolution struct {
	Status           string   `json:"status"`
	Method           string   `json:"method"`
	Confidence       string   `json:"confidence"`
	CandidateNodeIDs []string `json:"candidate_node_ids"`
}

type Port struct {
	InterfaceID string `json:"interface_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Number      uint32 `json:"number,omitempty"`
	Subtype     uint32 `json:"subtype"`
	RawID       string `json:"raw_id"`
}

type Edge struct {
	ID                    string         `json:"id"`
	AgentID               string         `json:"agent_id"`
	DeviceID              string         `json:"device_id"`
	From                  string         `json:"from"`
	To                    string         `json:"to"`
	LocalPort             Port           `json:"local_port"`
	RemotePort            Port           `json:"remote_port"`
	Protocol              string         `json:"protocol"`
	Source                string         `json:"source"`
	ObservedAt            time.Time      `json:"observed_at"`
	ReceivedAt            time.Time      `json:"received_at"`
	ExpiresAt             time.Time      `json:"expires_at"`
	Status                string         `json:"status"`
	ObservationConfidence string         `json:"observation_confidence"`
	Resolution            Resolution     `json:"resolution"`
	Evidence              []*pb.Evidence `json:"evidence"`
}

type Collection struct {
	AgentID          string    `json:"agent_id"`
	DeviceID         string    `json:"device_id"`
	MessageID        string    `json:"message_id"`
	Source           string    `json:"source"`
	Completeness     string    `json:"completeness"`
	ObservedAt       time.Time `json:"observed_at"`
	CollectedAt      time.Time `json:"collected_at"`
	ReceivedAt       time.Time `json:"received_at"`
	TTLSeconds       uint32    `json:"ttl_seconds"`
	ExpiryAssumption string    `json:"expiry_assumption"`
	ObservationCount int       `json:"observation_count"`
	ClockSkew        bool      `json:"clock_skew"`
}

type Graph struct {
	GeneratedAt        time.Time    `json:"generated_at"`
	Nodes              []Node       `json:"nodes"`
	Edges              []Edge       `json:"edges"`
	Collections        []Collection `json:"collections"`
	Truncated          bool         `json:"truncated"`
	InventoryTruncated bool         `json:"inventory_truncated"`
}

type resolver struct {
	byAgent map[string]map[string][]Device
}

func newResolver(devices []Device) resolver {
	r := resolver{byAgent: map[string]map[string][]Device{}}
	for _, d := range devices {
		if r.byAgent[d.AgentID] == nil {
			r.byAgent[d.AgentID] = map[string][]Device{}
		}
		index := r.byAgent[d.AgentID]
		add := func(kind, value string) {
			if value != "" {
				index[kind+":"+value] = append(index[kind+":"+value], d)
			}
		}
		add("name", normalizeName(d.Hostname))
		add("chassis", d.ChassisID)
		if mac, err := net.ParseMAC(d.ChassisID); err == nil {
			add("mac", mac.String())
		}
		if ip, err := netip.ParseAddr(d.ManagementAddress); err == nil {
			add("address", ip.Unmap().String())
		}
	}
	return r
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// Resolution is confined to the reporting Agent. A name match is explicitly
// tentative, and conflicting strong evidence is never resolved by a weak name.
func (r resolver) resolve(agentID string, n *pb.Neighbor) (Resolution, *Device) {
	result := Resolution{Status: "unresolved", Method: "none", Confidence: "unresolved", CandidateNodeIDs: []string{}}
	chassis, kind := chassisIdentity(n)
	var matches []Device
	if chassis != "" {
		matches = r.byAgent[agentID][kind+":"+chassis]
	}
	if len(matches) != 0 {
		result.Method, result.Confidence = "chassis_"+kind, "high"
	} else if name := normalizeName(n.RemoteSystemName); name != "" {
		matches = r.byAgent[agentID]["name:"+name]
		if len(matches) == 1 && contradicts(matches[0], chassis, kind) {
			result.Status, result.Method = "conflicting", "hostname_conflicts_with_chassis"
			result.CandidateNodeIDs = append(result.CandidateNodeIDs, DeviceNodeID(matches[0].AgentID, matches[0].ID))
			return result, nil
		}
		if len(matches) != 0 {
			result.Method, result.Confidence = "unique_hostname", "medium"
		}
	}
	for _, d := range matches {
		result.CandidateNodeIDs = append(result.CandidateNodeIDs, DeviceNodeID(d.AgentID, d.ID))
	}
	sort.Strings(result.CandidateNodeIDs)
	if len(matches) == 1 {
		result.Status = "resolved"
		return result, &matches[0]
	}
	if len(matches) > 1 {
		result.Status, result.Confidence = "ambiguous", "unresolved"
	}
	return result, nil
}

func contradicts(d Device, chassis, kind string) bool {
	if chassis == "" {
		return false
	}
	switch kind {
	case "mac":
		value, err := net.ParseMAC(d.ChassisID)
		return err == nil && !strings.EqualFold(value.String(), chassis)
	case "address":
		value, err := netip.ParseAddr(d.ManagementAddress)
		return err == nil && value.Unmap().String() != chassis
	case "chassis":
		return d.ChassisID != "" && d.ChassisID != chassis
	}
	return false
}

func chassisIdentity(n *pb.Neighbor) (string, string) {
	switch n.RemoteChassisSubtype {
	case 4: // LLDP chassis MAC.
		if len(n.RemoteChassisId) == 6 {
			return net.HardwareAddr(n.RemoteChassisId).String(), "mac"
		}
	case 5: // LLDP address family byte followed by IPv4/IPv6.
		if len(n.RemoteChassisId) == 5 && n.RemoteChassisId[0] == 1 || len(n.RemoteChassisId) == 17 && n.RemoteChassisId[0] == 2 {
			if addr, ok := netip.AddrFromSlice(n.RemoteChassisId[1:]); ok {
				return addr.Unmap().String(), "address"
			}
		}
	case 1, 2, 3, 6, 7:
		if utf8.Valid(n.RemoteChassisId) && len(n.RemoteChassisId) != 0 {
			return string(n.RemoteChassisId), "chassis"
		}
	}
	// RouterOS subtype 0 must not be silently reinterpreted as LLDP subtype 4.
	return "", ""
}

func nodeForDevice(d Device) Node {
	if d.Availability.Status == "" {
		d.Availability = availability.State{Status: "unknown", Reason: "no_observation", AgentConnected: d.AgentConnected}
	}
	label := d.Name
	if label == "" {
		label = d.Hostname
	}
	if label == "" {
		label = d.ID
	}
	seen := d.LastSeen.UTC()
	node := Node{ID: DeviceNodeID(d.AgentID, d.ID), AgentID: d.AgentID, DeviceID: d.ID, Known: true, Label: label, Hostname: d.Hostname,
		ManagementAddress: d.ManagementAddress, Vendor: d.Vendor, Model: d.Model, Type: d.Type, Source: d.Source, AgentConnected: &d.AgentConnected, LastSeen: &seen}
	if d.StateReported {
		node.ReportedOnline = &d.ReportedOnline
	}
	node.Availability = &d.Availability
	return node
}

func unresolvedNode(o Observation) Node {
	n := o.Neighbor
	id := digest("neighbor", o.AgentID, n.RemoteChassisSubtype, n.RemoteChassisId)
	if len(n.RemoteChassisId) == 0 {
		id = digest("neighbor", o.AgentID, o.ID)
	}
	label := n.RemoteSystemName
	if label == "" {
		label = "Unknown neighbor"
	}
	return Node{ID: id, AgentID: o.AgentID, Label: label, Source: o.Source, RemoteChassisSubtype: n.RemoteChassisSubtype, RemoteChassisID: base64.StdEncoding.EncodeToString(n.RemoteChassisId)}
}

// Build never mutates inventory or marks an unknown neighbor as online. The
// output is a directed observation graph, not an inferred physical cable list.
func Build(now time.Time, devices []Device, observations []Observation, collections []Collection, localDeviceID string, includeExpired, inventoryTruncated bool) Graph {
	g := Graph{GeneratedAt: now.UTC(), Nodes: []Node{}, Edges: []Edge{}, Collections: collections, InventoryTruncated: inventoryTruncated}
	if g.Collections == nil {
		g.Collections = []Collection{}
	}
	r := newResolver(devices)
	nodes := map[string]Node{}
	for _, d := range devices {
		if localDeviceID == "" || d.ID == localDeviceID {
			node := nodeForDevice(d)
			nodes[node.ID] = node
		}
	}
	for _, o := range observations {
		status := "active"
		if o.ObservedAt.After(now.Add(30 * time.Second)) {
			status = "clock_skew"
		} else if !o.ExpiresAt.After(now) {
			status = "expired"
		}
		if status != "active" && !includeExpired {
			continue
		}
		from := DeviceNodeID(o.AgentID, o.DeviceID)
		if _, ok := nodes[from]; !ok {
			nodes[from] = Node{ID: from, AgentID: o.AgentID, DeviceID: o.DeviceID, Known: true, Label: o.DeviceID, Source: o.Source}
		}
		resolution, target := r.resolve(o.AgentID, o.Neighbor)
		if inventoryTruncated {
			// Missing inventory could contain a second matching identity.
			resolution, target = Resolution{Status: "unresolved", Method: "inventory_truncated", Confidence: "unresolved", CandidateNodeIDs: []string{}}, nil
		}
		to := unresolvedNode(o)
		if target != nil {
			to = nodeForDevice(*target)
		}
		nodes[to.ID] = to
		n := o.Neighbor
		evidence := n.Evidence
		if evidence == nil {
			evidence = []*pb.Evidence{}
		}
		g.Edges = append(g.Edges, Edge{ID: o.ID, AgentID: o.AgentID, DeviceID: o.DeviceID, From: from, To: to.ID,
			LocalPort:  Port{InterfaceID: n.LocalInterfaceId, Name: n.LocalInterfaceName, Number: n.LocalPortNumber, Subtype: n.LocalPortSubtype, RawID: base64.StdEncoding.EncodeToString(n.LocalPortId)},
			RemotePort: Port{Name: n.RemotePortDescription, Subtype: n.RemotePortSubtype, RawID: base64.StdEncoding.EncodeToString(n.RemotePortId)},
			Protocol:   o.Protocol, Source: o.Source, ObservedAt: o.ObservedAt.UTC(), ReceivedAt: o.ReceivedAt.UTC(), ExpiresAt: o.ExpiresAt.UTC(),
			Status: status, ObservationConfidence: n.Confidence, Resolution: resolution, Evidence: evidence})
	}
	for _, node := range nodes {
		g.Nodes = append(g.Nodes, node)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.Slice(g.Edges, func(i, j int) bool { return g.Edges[i].ID < g.Edges[j].ID })
	return g
}
