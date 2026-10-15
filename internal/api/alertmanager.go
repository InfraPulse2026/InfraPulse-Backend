package api

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
	"net/http"
	"strings"
	"time"
)

type AlertmanagerRepository interface {
	AlertmanagerReceiver(context.Context, string) (prommodel.Receiver, error)
	SaveAlertmanagerReceiver(context.Context, string, prommodel.ReceiverInput) (prommodel.Receiver, error)
	CheckAlertmanagerToken(context.Context, string, string) error
	ReceiveAlertmanager(context.Context, string, string, prommodel.Webhook) (prommodel.WebhookResult, error)
}

func (a *API) alertmanagerFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, prommodel.ErrWebhookAuth):
		problem(w, 401, "invalid_webhook_token", "invalid source-scoped webhook credential")
	case errors.Is(err, prommodel.ErrWebhookConflict):
		problem(w, 409, "webhook_identity_conflict", "source revision or unique event identity required")
	case errors.Is(err, prommodel.ErrInvalid):
		problem(w, 400, "invalid_webhook", "invalid or truncated webhook payload")
	default:
		a.fail(w, err)
	}
}
func (a *API) alertmanagerConfig(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.dataSources.(AlertmanagerRepository)
	if !ok {
		problem(w, 503, "webhook_unavailable", "webhook storage unavailable")
		return
	}
	id := r.PathValue("id")
	if r.Method == "GET" {
		v, err := repo.AlertmanagerReceiver(r.Context(), id)
		if err != nil {
			a.alertmanagerFailure(w, err)
			return
		}
		write(w, 200, v)
		return
	}
	var in prommodel.ReceiverInput
	if !accessJSON(w, r, &in) {
		return
	}
	v, err := repo.SaveAlertmanagerReceiver(r.Context(), id, in)
	if err != nil {
		a.alertmanagerFailure(w, err)
		return
	}
	write(w, 200, v)
}
func (a *API) alertmanagerWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(r.Cookies()) != 0 {
		problem(w, 401, "invalid_webhook_token", "one source-scoped Bearer token is required")
		return
	}
	repo, ok := a.dataSources.(AlertmanagerRepository)
	if !ok || a.auth == nil {
		problem(w, 503, "webhook_unavailable", "webhook storage/auth unavailable")
		return
	}
	if !a.accessTransport(w, r) {
		return
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	id := r.PathValue("id")
	if err := repo.CheckAlertmanagerToken(r.Context(), id, token); err != nil {
		a.alertmanagerFailure(w, err)
		return
	}
	var in prommodel.Webhook
	if !boundedJSON(w, r, &in, 262144) {
		return
	}
	if err := in.Validate(time.Now().UTC()); err != nil {
		a.alertmanagerFailure(w, err)
		return
	}
	v, err := repo.ReceiveAlertmanager(r.Context(), id, token, in)
	if err != nil {
		a.alertmanagerFailure(w, err)
		return
	}
	write(w, 200, v)
}
