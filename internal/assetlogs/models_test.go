package assetlogs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
)

func TestPublicProjectionCannotExposeRawFields(t *testing.T) {
	r := logbook.Record{ID: 1, Host: "claimed-host", Raw: "stored-private-raw", Message: "sanitized message", Syslog: json.RawMessage(`{"peer_ip":"192.0.2.4","transport":"tls","authenticated":true,"raw_payload":"private-octets","nested":{"rawPayload":"private-nested"}}`), Observation: json.RawMessage(`{"source":{"collector_id":"reporter","target_id":"claimed-target","raw":"private-attribute"},"time":{"clock_offset":"0s"}}`)}
	b, err := json.Marshal(Public(r, "asset", "binding", "source", "session"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "private-") || strings.Contains(string(b), `"raw"`) || strings.Contains(string(b), `"raw_payload"`) || !strings.Contains(string(b), "claimed-host") || !strings.Contains(string(b), "192.0.2.4") || !strings.Contains(string(b), "claimed-target") {
		t.Fatal("public raw leak or reported attribution lost", string(b))
	}
	if r.Raw != "stored-private-raw" || !strings.Contains(string(r.Syslog), "private-octets") {
		t.Fatal("public projection mutated retained records")
	}
	legacy, err := json.Marshal(SafeLegacy(r))
	if err != nil || strings.Contains(string(legacy), "private-") || !strings.Contains(string(legacy), `"raw":""`) || r.Raw != "stored-private-raw" {
		t.Fatal("legacy safe projection changed format or exposed raw", string(legacy), err)
	}
	bad := Public(logbook.Record{Syslog: json.RawMessage(`invalid`)}, "a", "b", "s", "")
	if bad.Syslog != nil {
		t.Fatal("invalid metadata exposed")
	}
}

func TestFilterBoundsUnknownZeroFalseAndSimulationScope(t *testing.T) {
	now := time.Now().UTC()
	end := now.Add(time.Second)
	zero := uint32(0)
	no := false
	scope := ""
	valid := Filter{Log: logbook.Filter{Limit: 200, Severity: &zero, UID: &zero, Success: &no, From: &now, To: &end}, RecordScope: &scope}
	if err := ValidateFilter(valid); err != nil {
		t.Fatal("explicit real scope/zero/false rejected", err)
	}
	for _, f := range []Filter{
		{Log: logbook.Filter{Limit: 0}},
		{Log: logbook.Filter{Limit: 201}},
		{Log: logbook.Filter{Limit: 1, BeforeID: -1}},
		{Log: logbook.Filter{Limit: 1, Q: "word", Contains: "word"}},
		{Log: logbook.Filter{Limit: 1, From: &end, To: &now}},
		{Log: logbook.Filter{Limit: 1, Q: strings.Repeat("x", 257)}},
		{Log: logbook.Filter{Limit: 1, Severity: &zero, SeverityUnknown: true}},
		{Log: logbook.Filter{Limit: 1, Success: &no, SuccessUnknown: true}},
		{Log: logbook.Filter{Limit: 1}, SourceInstanceID: "hidden\x00source"},
	} {
		if ValidateFilter(f) == nil {
			t.Fatal("invalid filter accepted", f)
		}
	}
	if !ValidAssetID("asset_1") || ValidAssetID("") || ValidAssetID("with space") || ValidAssetID("hidden\x00id") {
		t.Fatal("asset identifier validation")
	}
}
