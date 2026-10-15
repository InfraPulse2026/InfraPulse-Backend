package topology

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type GlobalBinding struct {
	Asset                                                     assets.Asset
	BindingID, SourceID, SourceType, Scope, AgentID, DeviceID string
	SourceEnabled                                             bool
	FactID                                                    int64
	ObservedAt                                                time.Time
	Device                                                    *pb.Device
}
type GlobalObservation struct {
	Observation
	SourceID, Scope, MessageID string
	VersionID                  int64
}
type GlobalNode struct {
	ID         string   `json:"id"`
	AssetID    *string  `json:"asset_id"`
	Kind       string   `json:"kind"`
	AssetKind  string   `json:"asset_kind"`
	Label      string   `json:"label"`
	Scope      string   `json:"scope"`
	BindingIDs []string `json:"binding_ids"`
}
type GlobalEvidence struct {
	SourceID      string          `json:"source_instance_id"`
	AgentID       string          `json:"agent_id"`
	DeviceID      string          `json:"device_id"`
	Scope         string          `json:"scope"`
	ObservationID string          `json:"observation_id"`
	VersionID     int64           `json:"version_id"`
	Protocol      string          `json:"protocol"`
	Source        string          `json:"source"`
	From          string          `json:"from"`
	To            string          `json:"to"`
	LocalPort     Port            `json:"local_port"`
	RemotePort    Port            `json:"remote_port"`
	ObservedAt    time.Time       `json:"observed_at"`
	ReceivedAt    time.Time       `json:"received_at"`
	ExpiresAt     time.Time       `json:"expires_at"`
	Status        string          `json:"status"`
	Confidence    string          `json:"confidence"`
	Resolution    Resolution      `json:"resolution"`
	Neighbor      json.RawMessage `json:"neighbor"`
}
type GlobalEdge struct {
	ID           string           `json:"id"`
	Kind         string           `json:"kind"`
	From         string           `json:"from"`
	To           string           `json:"to"`
	FromPortKey  string           `json:"from_port_key"`
	ToPortKey    string           `json:"to_port_key"`
	Status       string           `json:"status"`
	Confirmation string           `json:"confirmation"`
	Confidence   string           `json:"confidence"`
	Protocols    []string         `json:"protocols"`
	Evidence     []GlobalEvidence `json:"evidence"`
}
type ManagementEvidence struct {
	SourceID         string    `json:"source_instance_id"`
	BindingID        string    `json:"binding_id"`
	FactID           int64     `json:"fact_id"`
	ObservedAt       time.Time `json:"observed_at"`
	TargetResourceID string    `json:"target_resource_id"`
}
type ManagementRelation struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	From     string               `json:"from"`
	To       string               `json:"to"`
	Status   string               `json:"status"`
	Evidence []ManagementEvidence `json:"evidence"`
}
type GlobalGraph struct {
	GeneratedAt           time.Time            `json:"generated_at"`
	Nodes                 []GlobalNode         `json:"nodes"`
	Edges                 []GlobalEdge         `json:"edges"`
	ManagementRelations   []ManagementRelation `json:"management_relations"`
	Truncated             bool                 `json:"truncated"`
	InventoryTruncated    bool                 `json:"inventory_truncated"`
	ObservationsTruncated bool                 `json:"observations_truncated"`
	TotalEdges            int                  `json:"total_edges"`
	Limit                 int                  `json:"limit"`
	Offset                int                  `json:"offset"`
}
type GlobalFilter struct {
	SourceID, AssetID, Scope string
	IncludeExpired           bool
	Limit, Offset            int
}

var ErrGlobalInvalid = errors.New("invalid global topology filter")
var ErrGlobalLimit = errors.New("global topology response exceeds bounds")

func GlobalNamespace(source, scope string) string {
	if scope == "" {
		return "real"
	}
	return "simulated:" + source + ":" + scope
}
func TopologyStatus(observed, expires, now time.Time) string {
	if observed.After(now.Add(30 * time.Second)) {
		return "clock_skew"
	}
	if !expires.After(now) {
		return "expired"
	}
	return "active"
}
func BusinessFingerprint(n *pb.Neighbor) string {
	c := proto.Clone(n).(*pb.Neighbor)
	c.ObservedAt = nil
	c.ExpiresAt = nil
	c.AgeSeconds = nil
	if c.Observation != nil {
		c.Observation.Time = nil
	}
	for _, i := range c.IdentityEvidence {
		if i != nil {
			i.Time = nil
		}
	}
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func globalOwnKey(source, scope, device string) string {
	return source + "\x00" + scope + "\x00" + device
}

type globalIndex struct {
	own       map[string][]GlobalBinding
	identity  map[string]map[string]GlobalBinding
	resources map[string][]GlobalBinding
}

func resourceKey(b GlobalBinding, resource string) string {
	s := b.Device.GetObservation().GetSource()
	return globalOwnKey(b.SourceID, b.Scope, s.GetTargetId()+"\x00"+s.GetType()+"\x00"+s.GetProtocol()+"\x00"+s.GetInstanceId()+"\x00"+resource)
}
func globalIndexes(bindings []GlobalBinding) globalIndex {
	r := globalIndex{own: map[string][]GlobalBinding{}, identity: map[string]map[string]GlobalBinding{}, resources: map[string][]GlobalBinding{}}
	for _, b := range bindings {
		if b.Device == nil {
			continue
		}
		r.own[globalOwnKey(b.SourceID, b.Scope, b.DeviceID)] = append(r.own[globalOwnKey(b.SourceID, b.Scope, b.DeviceID)], b)
		resource := b.Device.GetObservation().GetSource().GetResourceId()
		if resource != "" {
			k := resourceKey(b, resource)
			r.resources[k] = append(r.resources[k], b)
		}
		add := func(kind, value string) {
			if value == "" {
				return
			}
			k := GlobalNamespace(b.SourceID, b.Scope) + "\x00" + kind + ":" + value
			if r.identity[k] == nil {
				r.identity[k] = map[string]GlobalBinding{}
			}
			r.identity[k][b.Asset.ID] = b
		}
		add("name", normalizeName(b.Device.Hostname))
		identities := assets.Identities(b.Device)
		rejected := map[string]bool{}
		for _, i := range identities {
			if i.SubjectID == "" && !i.Usable && len(i.Evidence) > 0 {
				rejected[i.Kind+":"+i.NormalizedValue] = true
			}
		}
		if !rejected["chassis_id:"+b.Device.ChassisId] && !rejected["lldp_chassis_id:"+b.Device.ChassisId] {
			add("chassis", b.Device.ChassisId)
		}
		if mac, valid := assets.NormalizeIdentity("mac", b.Device.ChassisId); valid {
			if !rejected["mac:"+mac] {
				add("mac", mac)
			}
		}
		for _, i := range identities {
			if !i.Usable || i.SubjectID != "" {
				continue
			}
			switch i.Kind {
			case "mac":
				add("mac", i.NormalizedValue)
			case "management_ip":
				add("address", i.NormalizedValue)
			case "hostname", "sys_name":
				add("name", normalizeName(i.Value))
			case "chassis_id", "lldp_chassis_id":
				// Preserve subtype/raw chassis semantics instead of treating a text ID as MAC.
				var e pb.IdentityEvidence
				if len(i.Evidence) > 0 && protojson.Unmarshal(i.Evidence, &e) == nil && e.Subtype != nil {
					probe := &pb.Neighbor{RemoteChassisSubtype: *e.Subtype, RemoteChassisId: e.RawValue}
					v, k := chassisIdentity(probe)
					add(k, v)
				}
			}
		}
	}
	return r
}
func (r globalIndex) resolve(o GlobalObservation, truncated bool) (Resolution, *GlobalBinding) {
	result := Resolution{Status: "unresolved", Method: "none", Confidence: "unresolved", CandidateNodeIDs: []string{}}
	if truncated {
		result.Method = "inventory_truncated"
		return result, nil
	}
	value, kind := chassisIdentity(o.Neighbor)
	// MNDP native MAC is subtype 0; only its explicit protocol supplies semantics.
	if kind == "" && o.Neighbor.Protocol == pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_MNDP && o.Neighbor.RemoteChassisSubtype == 0 && len(o.Neighbor.RemoteChassisId) == 6 {
		mac := net.HardwareAddr(o.Neighbor.RemoteChassisId)
		if mac[0]&1 == 0 && !bytes.Equal(mac, make([]byte, 6)) {
			value, kind = mac.String(), "mac"
		}
	}
	candidates := r.identity[GlobalNamespace(o.SourceID, o.Scope)+"\x00"+kind+":"+value]
	strong := len(candidates) > 0
	if strong {
		result.Method = "global_chassis_" + kind
		result.Confidence = "high"
	} else {
		candidates = r.identity[GlobalNamespace(o.SourceID, o.Scope)+"\x00name:"+normalizeName(o.Neighbor.RemoteSystemName)]
		if len(candidates) > 0 {
			result.Method = "hostname_candidate"
			result.Confidence = "low"
		}
	}
	for id := range candidates {
		result.CandidateNodeIDs = append(result.CandidateNodeIDs, id)
	}
	sort.Strings(result.CandidateNodeIDs)
	if len(candidates) > 1 {
		result.Status = "ambiguous"
		result.Confidence = "unresolved"
		return result, nil
	}
	for _, b := range candidates {
		if b.Asset.ResolutionStatus == "conflict" {
			result.Status = "conflicting"
			result.Method = "asset_identity_conflict"
			return result, nil
		}
		if !strong || kind == "address" {
			if !strong && contradicts(Device{ChassisID: b.Device.ChassisId, ManagementAddress: b.Device.ManagementAddress}, value, kind) {
				result.Status = "conflicting"
				result.Method = "hostname_conflicts_with_chassis"
				return result, nil
			}
			result.Status = "tentative"
			result.Confidence = "low"
			return result, nil
		}
		result.Status = "resolved"
		return result, &b
	}
	return result, nil
}
func stablePort(n *pb.Neighbor, local bool, owner string) (string, bool) {
	subtype, raw := n.RemotePortSubtype, n.RemotePortId
	if local {
		subtype, raw = n.LocalPortSubtype, n.LocalPortId
	}
	if len(raw) > 0 && subtype >= 1 && subtype <= 7 {
		protocol := n.Protocol.String()
		if n.Protocol == pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_LLDP {
			protocol = "LLDP"
		}
		return digest("port", owner, protocol, subtype, raw), true
	}
	// Interface ID/index/name is local to the source and cannot prove reciprocal ports.
	return "", false
}
func globalAssetNode(b GlobalBinding) GlobalNode {
	id := b.Asset.ID
	return GlobalNode{ID: id, AssetID: &id, Kind: "asset", AssetKind: b.Asset.Kind, Label: b.Asset.CanonicalName, Scope: b.Asset.Scope, BindingIDs: []string{b.BindingID}}
}
func appendUnique(values []string, v string) []string {
	for _, old := range values {
		if old == v {
			return values
		}
	}
	return append(values, v)
}

// BuildGlobal folds evidence only after devices resolve to existing Assets and
// advertised typed port IDs match. Bidirectionality confirms L2 observations,
// not a physical cable, device health or automatic Asset merging.
func BuildGlobal(now time.Time, bindings []GlobalBinding, observations []GlobalObservation, filter GlobalFilter, inventoryTruncated, observationsTruncated bool) GlobalGraph {
	assetID, includeExpired, limit, offset := filter.AssetID, filter.IncludeExpired, filter.Limit, filter.Offset
	g := GlobalGraph{GeneratedAt: now.UTC(), Nodes: []GlobalNode{}, Edges: []GlobalEdge{}, ManagementRelations: []ManagementRelation{}, InventoryTruncated: inventoryTruncated, ObservationsTruncated: observationsTruncated, Limit: limit, Offset: offset}
	index := globalIndexes(bindings)
	nodes := map[string]GlobalNode{}
	edges := map[string]*GlobalEdge{}
	directions := map[string]map[string]bool{}
	for _, b := range bindings {
		node := globalAssetNode(b)
		if old, ok := nodes[node.ID]; ok {
			node.BindingIDs = appendUnique(old.BindingIDs, b.BindingID)
		}
		nodes[node.ID] = node
	}
	for _, o := range observations {
		if filter.SourceID != "" && filter.SourceID != o.SourceID {
			continue
		}
		status := TopologyStatus(o.ObservedAt, o.ExpiresAt, now)
		if status != "active" && !includeExpired {
			continue
		}
		own := index.own[globalOwnKey(o.SourceID, o.Scope, o.DeviceID)]
		from := digest("source-device", o.SourceID, o.Scope, o.DeviceID)
		localKnown := len(own) == 1 && own[0].Asset.ResolutionStatus != "conflict"
		if localKnown {
			from = own[0].Asset.ID
		} else {
			nodes[from] = GlobalNode{ID: from, Kind: "source_device", Label: o.DeviceID, Scope: GlobalNamespace(o.SourceID, o.Scope), BindingIDs: []string{}}
		}
		resolution, target := index.resolve(o, inventoryTruncated)
		to := digest("unknown-neighbor", o.SourceID, o.Scope, o.Protocol, o.Neighbor.RemoteChassisSubtype, o.Neighbor.RemoteChassisId)
		if len(o.Neighbor.RemoteChassisId) == 0 {
			to = digest("unknown-neighbor", o.SourceID, o.Scope, o.ID)
		}
		if target != nil {
			to = target.Asset.ID
		} else {
			label := o.Neighbor.RemoteSystemName
			if label == "" {
				label = "Unknown neighbor"
			}
			nodes[to] = GlobalNode{ID: to, Kind: "unresolved_neighbor", Label: label, Scope: GlobalNamespace(o.SourceID, o.Scope), BindingIDs: []string{}}
		}
		if assetID != "" && from != assetID && to != assetID {
			continue
		}
		lp, lok := stablePort(o.Neighbor, true, from)
		rp, rok := stablePort(o.Neighbor, false, to)
		if !lok {
			lp = digest("source-port", o.SourceID, o.Scope, o.Neighbor.LocalInterfaceId, o.Neighbor.LocalPortNumber, o.ID)
		}
		if !rok {
			rp = digest("unknown-port", o.SourceID, o.Scope, o.ID)
		}
		left, right, leftPort, rightPort := from, to, lp, rp
		if left+"\x00"+leftPort > right+"\x00"+rightPort {
			left, right, leftPort, rightPort = right, left, rightPort, leftPort
		}
		id := digest("global-edge", left, leftPort, right, rightPort)
		edge := edges[id]
		if edge == nil {
			edge = &GlobalEdge{ID: id, Kind: "layer2_observation", From: left, To: right, FromPortKey: leftPort, ToPortKey: rightPort, Status: status, Confirmation: "one_sided", Confidence: "unresolved", Protocols: []string{}, Evidence: []GlobalEvidence{}}
			edges[id] = edge
			directions[id] = map[string]bool{}
		}
		n := o.Neighbor
		edge.Protocols = appendUnique(edge.Protocols, o.Protocol)
		edge.Evidence = append(edge.Evidence, GlobalEvidence{SourceID: o.SourceID, AgentID: o.AgentID, DeviceID: o.DeviceID, Scope: o.Scope, ObservationID: o.ID, VersionID: o.VersionID, Protocol: o.Protocol, Source: o.Source, From: from, To: to, LocalPort: Port{InterfaceID: n.LocalInterfaceId, Name: n.LocalInterfaceName, Number: n.LocalPortNumber, Subtype: n.LocalPortSubtype, RawID: base64.StdEncoding.EncodeToString(n.LocalPortId)}, RemotePort: Port{Name: n.RemotePortDescription, Subtype: n.RemotePortSubtype, RawID: base64.StdEncoding.EncodeToString(n.RemotePortId)}, ObservedAt: o.ObservedAt, ReceivedAt: o.ReceivedAt, ExpiresAt: o.ExpiresAt, Status: status, Confidence: n.Confidence, Resolution: resolution, Neighbor: neighborJSON(n)})
		if status == "active" {
			edge.Status = "active"
			if localKnown && target != nil && lok && rok && from != to {
				directions[id][from+"\x00"+lp] = true
			}
		}
	}
	for _, edge := range edges {
		if len(directions[edge.ID]) == 2 {
			edge.Confirmation = "bidirectional"
		}
		for _, e := range edge.Evidence {
			if e.Status == "active" && e.Resolution.Status != "resolved" {
				edge.Confirmation = "unresolved"
			}
		}
		conf := "high"
		active := false
		for _, e := range edge.Evidence {
			if e.Status != "active" {
				continue
			}
			active = true
			if confidenceRank(e.Confidence) < confidenceRank(conf) {
				conf = e.Confidence
			}
			if e.Resolution.Confidence != "high" {
				conf = "unresolved"
			}
		}
		if active && (conf == "high" || conf == "medium" || conf == "low") {
			edge.Confidence = conf
		}
		sort.Strings(edge.Protocols)
		g.Edges = append(g.Edges, *edge)
	}
	sort.Slice(g.Edges, func(i, j int) bool { return g.Edges[i].ID < g.Edges[j].ID })
	g.TotalEdges = len(g.Edges)
	start := min(offset, len(g.Edges))
	end := min(start+limit, len(g.Edges))
	g.Edges = g.Edges[start:end]
	used := map[string]bool{}
	for _, e := range g.Edges {
		used[e.From] = true
		used[e.To] = true
	}
	reporters := []GlobalBinding{}
	for _, b := range bindings {
		if filter.SourceID == "" || filter.SourceID == b.SourceID {
			reporters = append(reporters, b)
		}
	}
	g.ManagementRelations = globalManagement(reporters, index, nodes, assetID, inventoryTruncated)
	if len(g.ManagementRelations) > 2000 {
		g.ManagementRelations = g.ManagementRelations[:2000]
		g.Truncated = true
	}
	for _, rel := range g.ManagementRelations {
		used[rel.From] = true
		used[rel.To] = true
	}
	if assetID != "" {
		used[assetID] = true
	}
	if assetID == "" && offset == 0 && g.TotalEdges <= limit {
		for _, b := range reporters {
			used[b.Asset.ID] = true
		}
	}
	for id := range used {
		if node, ok := nodes[id]; ok {
			sort.Strings(node.BindingIDs)
			g.Nodes = append(g.Nodes, node)
		}
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	g.Truncated = g.Truncated || inventoryTruncated || observationsTruncated || offset > 0 || end < g.TotalEdges
	return g
}
func confidenceRank(v string) int {
	switch v {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}
func neighborJSON(n *pb.Neighbor) json.RawMessage {
	raw, _ := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(n)
	return raw
}
func globalManagement(bindings []GlobalBinding, index globalIndex, nodes map[string]GlobalNode, assetID string, truncated bool) []ManagementRelation {
	relations := map[string]*ManagementRelation{}
	for _, b := range bindings {
		if b.Device == nil {
			continue
		}
		for _, reported := range b.Device.GetHardware().GetRelations() {
			typ := reported.Type
			if typ != "BMC_MANAGES_SYSTEM" && typ != "BMC_MANAGES_CHASSIS" && typ != "MANAGED_BY_BMC" {
				continue
			}
			from, to := b.Asset.ID, digest("unknown-resource", resourceKey(b, reported.TargetResourceId))
			candidates := index.resources[resourceKey(b, reported.TargetResourceId)]
			unique := map[string]GlobalBinding{}
			for _, c := range candidates {
				unique[c.Asset.ID] = c
			}
			var target *GlobalBinding
			if len(unique) == 1 && !truncated {
				for _, c := range unique {
					copy := c
					target = &copy
				}
			}
			status := "unresolved"
			if len(unique) > 1 {
				status = "ambiguous"
			}
			if typ == "MANAGED_BY_BMC" {
				if target == nil || target.Asset.Kind != "bmc" || b.Asset.Kind != "system" && b.Asset.Kind != "host" && b.Asset.Kind != "chassis" {
					continue
				}
				from, to = target.Asset.ID, b.Asset.ID
				typ = "BMC_MANAGES_SYSTEM"
				if b.Asset.Kind == "chassis" {
					typ = "BMC_MANAGES_CHASSIS"
				}
				status = "resolved"
			} else {
				if b.Asset.Kind != "bmc" {
					continue
				}
				if target != nil {
					valid := typ == "BMC_MANAGES_SYSTEM" && (target.Asset.Kind == "system" || target.Asset.Kind == "host") || typ == "BMC_MANAGES_CHASSIS" && target.Asset.Kind == "chassis"
					if valid {
						to = target.Asset.ID
						status = "resolved"
					} else {
						target = nil
					}
				}
				if target == nil {
					nodes[to] = GlobalNode{ID: to, Kind: "unresolved_resource", Label: reported.TargetResourceId, Scope: GlobalNamespace(b.SourceID, b.Scope), BindingIDs: []string{}}
				}
			}
			if assetID != "" && from != assetID && to != assetID {
				continue
			}
			id := digest("management-relation", typ, from, to)
			rel := relations[id]
			if rel == nil {
				rel = &ManagementRelation{ID: id, Type: typ, From: from, To: to, Status: status, Evidence: []ManagementEvidence{}}
				relations[id] = rel
			}
			rel.Evidence = append(rel.Evidence, ManagementEvidence{SourceID: b.SourceID, BindingID: b.BindingID, FactID: b.FactID, ObservedAt: b.ObservedAt, TargetResourceID: reported.TargetResourceId})
		}
	}
	out := []ManagementRelation{}
	for _, r := range relations {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
