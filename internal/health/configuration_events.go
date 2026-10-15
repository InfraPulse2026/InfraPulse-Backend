package health

// Evidence contains immutable references and counts, never configuration text.
type ConfigurationEvidence struct {
	AssetID          string `json:"asset_id"`
	AssetRevision    int64  `json:"asset_revision"`
	BaselineRevision int64  `json:"baseline_revision"`
	TargetRevision   int64  `json:"target_revision"`
	ResultID         int64  `json:"result_id"`
	SnapshotID       string `json:"snapshot_id"`
	Normalizer       string `json:"normalizer"`
	TotalChanges     int    `json:"total_changes"`
	Truncated        bool   `json:"truncated"`
}
