// Package topology projects Agent observations without inventing device identity.
package topology

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
)

const DefaultTTLSeconds = 900
const MaxTTLSeconds = 86400
const MaxObservations = 10000

// Observation preserves Agent-owned port/chassis bytes and discovery evidence.
type Observation struct {
	ID         string
	AgentID    string
	DeviceID   string
	Source     string
	Protocol   string
	ObservedAt time.Time
	ReceivedAt time.Time
	ExpiresAt  time.Time
	Neighbor   *pb.Neighbor
}

func Validate(t *pb.TopologyUpdate) error {
	if t == nil || t.DeviceId == "" || len(t.DeviceId) > 256 || t.Source == "" || len(t.Source) > 256 || t.ObservedAt == nil {
		return errors.New("topology requires device_id, source and observed_at")
	}
	if err := t.ObservedAt.CheckValid(); err != nil {
		return errors.New("invalid topology observed_at")
	}
	if t.CollectedAt != nil && t.CollectedAt.CheckValid() != nil {
		return errors.New("invalid topology collected_at")
	}
	if t.TtlSeconds > MaxTTLSeconds || len(t.Observations) > MaxObservations {
		return errors.New("topology exceeds TTL or observation limit")
	}
	if t.Completeness < pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_UNSPECIFIED || t.Completeness > pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_FAILED {
		return errors.New("unsupported topology completeness")
	}
	for _, n := range t.Observations {
		if n == nil || (n.LocalDeviceId != "" && n.LocalDeviceId != t.DeviceId) {
			return errors.New("neighbor must belong to topology device")
		}
		if n.ObservedAt != nil && n.ObservedAt.CheckValid() != nil || n.ExpiresAt != nil && n.ExpiresAt.CheckValid() != nil {
			return errors.New("invalid neighbor timestamp")
		}
		observed := t.ObservedAt.AsTime()
		if n.ObservedAt != nil {
			observed = n.ObservedAt.AsTime()
		}
		if n.ExpiresAt != nil && (n.ExpiresAt.AsTime().Before(observed) || n.ExpiresAt.AsTime().Sub(observed) > MaxTTLSeconds*time.Second) {
			return errors.New("neighbor expiry must be within 24 hours of observation")
		}
		if n.ExpiresAt == nil {
			ttl := t.TtlSeconds
			if ttl == 0 {
				ttl = DefaultTTLSeconds
			}
			if observed.Add(time.Duration(ttl)*time.Second).Year() > 9999 {
				return errors.New("derived neighbor expiry exceeds timestamp range")
			}
		}
	}
	return nil
}

func Prepare(agentID string, t *pb.TopologyUpdate, received time.Time) ([]Observation, error) {
	if err := Validate(t); err != nil {
		return nil, err
	}
	items := make([]Observation, 0, len(t.Observations))
	// A failed poll provides collection diagnostics, never new link evidence.
	if t.Completeness == pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_FAILED {
		return items, nil
	}
	ttl := t.TtlSeconds
	if ttl == 0 {
		ttl = DefaultTTLSeconds
	}
	for _, n := range t.Observations {
		observed := t.ObservedAt.AsTime().UTC()
		if n.ObservedAt != nil {
			observed = n.ObservedAt.AsTime().UTC()
		}
		expires := observed.Add(time.Duration(ttl) * time.Second)
		if n.ExpiresAt != nil {
			expires = n.ExpiresAt.AsTime().UTC()
		}
		source := n.Source
		if source == "" {
			source = t.Source
		}
		nameIdentity := ""
		if len(n.RemoteChassisId) == 0 {
			nameIdentity = n.RemoteSystemName
		}
		id := digest("edge", agentID, t.DeviceId, n.LocalInterfaceId, n.LocalPortNumber, n.LocalPortSubtype, n.LocalPortId,
			n.RemoteChassisSubtype, n.RemoteChassisId, n.RemotePortSubtype, n.RemotePortId, nameIdentity, n.Protocol, source)
		items = append(items, Observation{ID: id, AgentID: agentID, DeviceID: t.DeviceId, Source: source, Protocol: n.Protocol.String(), ObservedAt: observed, ReceivedAt: received.UTC(), ExpiresAt: expires, Neighbor: n})
	}
	return items, nil
}

func digest(prefix string, values ...any) string {
	data, err := json.Marshal(values)
	if err != nil {
		panic(fmt.Sprintf("internal topology identity: %v", err))
	}
	hash := sha256.Sum256(data)
	return prefix + ":" + hex.EncodeToString(hash[:])
}

func DeviceNodeID(agentID, deviceID string) string { return digest("device", agentID, deviceID) }
