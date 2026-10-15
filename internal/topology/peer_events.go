package topology

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"time"
)

type PeerEndpoint struct {
	PeerHash, Protocol    string
	ObservedAt, ExpiresAt time.Time
}

func peerHash(values ...any) string {
	raw, _ := json.Marshal(values)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func PeerEventKey(binding, source, scope, port string) string {
	return peerHash(binding, source, scope, port)
}

func reliableClock(m *pb.ObservationMetadata) bool {
	q := m.GetTime().GetClockQuality()
	return q != "unsynced" && q != "degraded"
}

// Only two complete, unambiguous, fresh snapshots can report a peer replacement.
// Ports absent from either snapshot are deliberately not interpreted as failures.
func PeerSnapshot(t *pb.TopologyUpdate, received time.Time) (map[string]PeerEndpoint, bool) {
	if t == nil || t.Completeness != pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_COMPLETE || len(t.Observations) > 512 || !reliableClock(t.GetObservation()) {
		return nil, false
	}
	observations, err := Prepare("", t, received)
	if err != nil {
		return nil, false
	}
	batch := t.ObservedAt.AsTime()
	if batch.Before(time.Unix(0, 0)) || received.Sub(batch) > 5*time.Minute || batch.After(received.Add(30*time.Second)) {
		return nil, false
	}
	out := map[string]PeerEndpoint{}
	for _, o := range observations {
		n := o.Neighbor
		if (n.Protocol != pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_LLDP && n.Protocol != pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_CDP) || n.Confidence != "high" || !reliableClock(n.GetObservation()) || n.LocalPortSubtype < 1 || n.LocalPortSubtype > 7 || len(n.LocalPortId) == 0 || len(n.LocalPortId) > 256 || n.RemoteChassisSubtype < 1 || n.RemoteChassisSubtype > 7 || len(n.RemoteChassisId) == 0 || len(n.RemoteChassisId) > 256 || n.RemotePortSubtype < 1 || n.RemotePortSubtype > 7 || len(n.RemotePortId) == 0 || len(n.RemotePortId) > 256 || o.ObservedAt.Before(time.Unix(0, 0)) || received.Sub(o.ObservedAt) > 5*time.Minute || o.ObservedAt.After(received.Add(30*time.Second)) || !o.ExpiresAt.After(received) {
			return nil, false
		}
		if n.RemoteChassisSubtype == 4 && len(n.RemoteChassisId) != 6 {
			return nil, false
		}
		for _, m := range []*pb.ObservationMetadata{t.GetObservation(), n.GetObservation()} {
			if at := m.GetTime().GetSourceTimestamp(); at != nil && (at.CheckValid() != nil || received.Sub(at.AsTime()) > 5*time.Minute || at.AsTime().After(received.Add(30*time.Second))) {
				return nil, false
			}
		}
		// A changed target/profile is a new observation context, not evidence
		// of a physical peer replacement. Resource IDs may describe a peer,
		// so they deliberately do not define the local-port key.
		root, leaf := t.GetObservation().GetSource(), n.GetObservation().GetSource()
		key := peerHash(n.Protocol, n.LocalPortSubtype, n.LocalPortId, o.Source,
			root.GetType(), root.GetTargetId(), root.GetProtocol(), root.GetInstanceId(),
			leaf.GetType(), leaf.GetTargetId(), leaf.GetProtocol(), leaf.GetInstanceId())
		if _, exists := out[key]; exists {
			return nil, false
		}
		out[key] = PeerEndpoint{PeerHash: peerHash(n.RemoteChassisSubtype, n.RemoteChassisId, n.RemotePortSubtype, n.RemotePortId), Protocol: o.Protocol, ObservedAt: o.ObservedAt, ExpiresAt: o.ExpiresAt}
	}
	return out, true
}
