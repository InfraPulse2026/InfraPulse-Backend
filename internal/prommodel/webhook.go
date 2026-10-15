package prommodel

import (
	"errors"
	"time"
)

var ErrWebhookAuth = errors.New("invalid webhook credentials")
var ErrWebhookConflict = errors.New("webhook source revision or event identity conflict")

type ReceiverInput struct {
	Enabled          bool   `json:"enabled"`
	ExpectedRevision int64  `json:"expected_revision"`
	TTLSeconds       int    `json:"ttl_seconds"`
	Confirmed        bool   `json:"confirmed"`
	Reason           string `json:"reason"`
}
type Receiver struct {
	SourceID       string    `json:"source_id"`
	Revision       int64     `json:"revision"`
	SourceRevision int64     `json:"source_revision"`
	Enabled        bool      `json:"enabled"`
	ExpiresAt      time.Time `json:"expires_at"`
	Token          string    `json:"token,omitempty"`
}
type WebhookAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}
type Webhook struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Receiver          string            `json:"receiver"`
	Status            string            `json:"status"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []WebhookAlert    `json:"alerts"`
}
type WebhookResult struct {
	Resolved int `json:"resolved"`
	Ignored  int `json:"ignored"`
}

func (w Webhook) Validate(now time.Time) error {
	if w.Version != "4" || w.TruncatedAlerts != 0 || len(w.Alerts) > 100 || w.Status != "firing" && w.Status != "resolved" {
		return ErrInvalid
	}
	for _, a := range w.Alerts {
		if a.Status != "firing" && a.Status != "resolved" || len(a.Labels) > 64 {
			return ErrInvalid
		}
		for k, v := range a.Labels {
			if !Text(k, 128) || !Text(v, 1024) {
				return ErrInvalid
			}
		}
		if a.Status == "resolved" && (a.StartsAt.IsZero() || a.EndsAt.IsZero() || a.StartsAt.Year() < 1970 || a.EndsAt.Before(a.StartsAt) || a.EndsAt.After(now.Add(30*time.Second))) {
			return ErrInvalid
		}
	}
	return nil
}
