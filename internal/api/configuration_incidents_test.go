package api

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
)

func TestConfigurationIncidentFilterAcceptsAssetResource(t *testing.T) {
	h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDevelopmentAuthDisabled(), WithIncidents(incidentStub{}))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/api/incidents?resource_kind=configuration&resource_id=asset_switch", nil))
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
}
