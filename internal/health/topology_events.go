package health

import "time"

type TopologyEvidence struct {
	BindingID        string     `json:"binding_id"`
	BeforeSnapshotID int64      `json:"before_snapshot_id"`
	AfterSnapshotID  int64      `json:"after_snapshot_id"`
	LocalPortHash    string     `json:"local_port_hash"`
	BeforePeerHash   string     `json:"before_peer_hash"`
	AfterPeerHash    string     `json:"after_peer_hash"`
	Protocol         string     `json:"protocol"`
	EventCount       int64      `json:"event_count"`
	ExpiresAt        time.Time  `json:"expires_at"`
	Lifecycle        string     `json:"lifecycle"`
	ReviewedAt       *time.Time `json:"reviewed_at,omitempty"`
	ReviewedBy       string     `json:"reviewed_by,omitempty"`
	ReviewReason     string     `json:"review_reason,omitempty"`
}

type TopologyIncidentRecord struct {
	ID               int64     `json:"id"`
	BeforeSnapshotID int64     `json:"before_snapshot_id"`
	AfterSnapshotID  int64     `json:"after_snapshot_id"`
	BindingID        string    `json:"binding_id"`
	LocalPortHash    string    `json:"local_port_hash"`
	ObservedAt       time.Time `json:"observed_at"`
}
