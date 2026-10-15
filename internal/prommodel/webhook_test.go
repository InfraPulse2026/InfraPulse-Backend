package prommodel

import (
	"testing"
	"time"
)

func TestWebhookExplicitRecoveryAndTruncation(t *testing.T) {
	now := time.Now().UTC()
	good := Webhook{Version: "4", Status: "resolved", Alerts: []WebhookAlert{{Status: "resolved", StartsAt: now.Add(-time.Minute), EndsAt: now, Labels: map[string]string{"alertname": "CPU"}}}}
	if err := good.Validate(now); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.TruncatedAlerts = 1
	if bad.Validate(now) == nil {
		t.Fatal("truncated payload accepted")
	}
	bad = good
	bad.Alerts = append([]WebhookAlert{}, good.Alerts...)
	bad.Alerts[0].EndsAt = now.Add(time.Hour)
	if bad.Validate(now) == nil {
		t.Fatal("future recovery accepted")
	}
}
