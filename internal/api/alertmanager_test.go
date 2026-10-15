package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

type amStub struct {
	DataSourceRepository
	received int
}

func (s *amStub) AlertmanagerReceiver(context.Context, string) (prommodel.Receiver, error) {
	return prommodel.Receiver{Revision: 1}, nil
}
func (s *amStub) SaveAlertmanagerReceiver(context.Context, string, prommodel.ReceiverInput) (prommodel.Receiver, error) {
	return prommodel.Receiver{Revision: 2, Token: "only-returned-on-rotation"}, nil
}
func (s *amStub) CheckAlertmanagerToken(_ context.Context, id, token string) error {
	if id != "source" || token != "source-token" {
		return prommodel.ErrWebhookAuth
	}
	return nil
}
func (s *amStub) ReceiveAlertmanager(context.Context, string, string, prommodel.Webhook) (prommodel.WebhookResult, error) {
	s.received++
	return prommodel.WebhookResult{Resolved: 1}, nil
}
func TestAlertmanagerHTTPScopedAuthAndClosedBody(t *testing.T) {
	token, _ := access.NewToken()
	hash, _ := access.TokenHash(token)
	stub := &amStub{}
	auth := access.New(accessStub{principal: access.Principal{User: access.User{Role: "viewer"}, SessionHash: hash}}, access.Config{Mode: "required"})
	h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(auth), WithDataSources(stub))
	send := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := send("GET", "/api/data-sources/source/alertmanager", "", token); w.Code != 403 {
		t.Fatal("viewer config", w.Code)
	}
	valid := `{"version":"4","status":"resolved","alerts":[{"status":"resolved","labels":{"alertname":"CPU"},"startsAt":"2026-01-01T00:00:00Z","endsAt":"2026-01-01T01:00:00Z"}]}`
	for _, credential := range []string{"", token, "wrong"} {
		if w := send("POST", "/api/data-sources/source/alertmanager/webhook", valid, credential); w.Code != 401 {
			t.Fatal("privileged user token accepted as source token", w.Code)
		}
	}
	if w := send("POST", "/api/data-sources/source/alertmanager/webhook", valid, "source-token"); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	before := stub.received
	for _, body := range []string{`{"version":"4","status":"resolved","unexpected":1}`, `{"version":"4","status":"resolved","truncatedAlerts":1}`, `{"version":"4","status":"resolved","alerts":[{"status":"resolved"}]}`} {
		if w := send("POST", "/api/data-sources/source/alertmanager/webhook", body, "source-token"); w.Code != 400 {
			t.Fatal("invalid body", w.Code)
		}
	}
	if stub.received != before {
		t.Fatal("invalid payload reached storage")
	}
}
