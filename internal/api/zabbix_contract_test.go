package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
)

func TestZabbixSourceConfigurationHTTPAndOpenAPI(t *testing.T) {
	token, _ := access.NewToken()
	hash, _ := access.TokenHash(token)
	stub := accessStub{principal: access.Principal{User: access.User{ID: "fixture", Role: "admin"}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
	repo := &sourceStub{}
	h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(access.New(stub, access.Config{Mode: "required", SessionTTL: time.Hour})), WithDataSources(repo))
	for _, tc := range []struct {
		selection string
		want      int
	}{
		{`{"host_ids":["2","1"],"page_size":1,"device_type":"switch","verified_inventory":false}`, 201},
		{`{"host_ids":["01"]}`, 400},
		{`{"host_ids":["1","1"]}`, 400},
		{`{"host_ids":["1"],"page_size":51}`, 400},
		{`{"host_ids":["1"],"allow_all_hosts":true}`, 400},
	} {
		body := `{"type":"ZABBIX","name":"fixture","endpoint":"https://monitor.example/api_jsonrpc.php","enabled":true,"sync_interval_seconds":3,"configuration":{"zabbix":` + tc.selection + `}}`
		request := httptest.NewRequest("POST", "/api/data-sources", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, request)
		if recorder.Code != tc.want {
			t.Fatal("configuration contract", recorder.Code, tc.want, recorder.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/openapi.json", nil))
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if json.Unmarshal(w.Body.Bytes(), &spec) != nil {
		t.Fatal("invalid OpenAPI")
	}
	for _, name := range []string{"DataSource", "DataSourceInput"} {
		if !strings.Contains(string(spec.Components.Schemas[name].Properties["configuration"]), "ZabbixSettings") {
			t.Fatal("source config omitted Zabbix settings", name)
		}
	}
	for _, name := range []string{"AssetSourceBinding", "AssetFactVersion"} {
		if spec.Components.Schemas[name].Properties["provider_state"] == nil {
			t.Fatal("provider state omitted", name)
		}
	}
	if spec.Components.Schemas["ZabbixSettings"].Properties["verified_inventory"] == nil {
		t.Fatal("inventory trust not documented")
	}
}
