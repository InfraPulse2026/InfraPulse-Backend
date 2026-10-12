package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/api"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/zabbix"
	"github.com/jackc/pgx/v5"
)

func TestZabbixMonitoringTLSQueueAndAuthorizedREST(t *testing.T) {
	db := os.Getenv("TEST_DATABASE_URL")
	if db == "" {
		t.Skip("set loopback TEST_DATABASE_URL")
	}
	parsed, err := url.Parse(db)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatal("requires disposable loopback database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := pgx.Identifier{fmt.Sprintf("infrapulse_zabbix_%d", time.Now().UnixNano())}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	q := parsed.Query()
	q.Set("search_path", strings.Trim(schema, "\""))
	parsed.RawQuery = q.Encode()
	store, err := storage.Open(ctx, parsed.String(), 15*time.Second, monitoring.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const token = "fixture-private-zabbix-token"
	var mode atomic.Int32
	now := time.Now().UTC().Truncate(time.Second)
	problemEvent := func(recovery string) map[string]string {
		return map[string]string{"eventid": "11", "objectid": "700", "source": "0", "object": "0", "value": "1", "clock": strconv.FormatInt(now.Add(-time.Hour).Unix(), 10), "ns": "0", "name": "CPU high", "severity": "4", "acknowledged": "0", "suppressed": "0", "r_eventid": recovery}
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string          `json:"method"`
			ID     int             `json:"id"`
			Auth   string          `json:"auth"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid request")
			w.WriteHeader(400)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api_jsonrpc.php" {
			t.Error("wrong API endpoint")
		}
		if request.Method == "apiinfo.version" {
			if r.Header.Get("Authorization") != "" || request.Auth != "" {
				t.Error("version leaked token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": "7.4.15"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token || request.Auth != "" {
			t.Error("invalid authenticated RPC")
		}
		if mode.Load() == 1 && request.Method != "host.get" {
			w.WriteHeader(503)
			return
		}
		result := []any{}
		switch request.Method {
		case "host.get":
			result = []any{map[string]any{"hostid": "101", "host": "switch-101", "name": "Eltex lab", "status": "0", "maintenance_status": "0", "maintenance_type": "0", "active_available": "0", "inventory": map[string]string{"vendor": "Eltex", "model": "MES2324B", "serialno_a": "LAB-101"}, "interfaces": []any{}, "hostgroups": []any{}, "tags": []any{}}}
		case "item.get":
			result = []any{map[string]string{"itemid": "901", "hostid": "101", "name": "CPU", "units": "%", "value_type": "0", "status": "0", "state": "0", "lastclock": strconv.FormatInt(now.Unix(), 10), "lastns": "0", "lastvalue": "82.125"}}
		case "problem.get":
			if mode.Load() == 0 {
				result = []any{problemEvent("0")}
			}
		case "event.get":
			var params struct {
				IDs      []string `json:"eventids"`
				From     string   `json:"eventid_from"`
				TimeFrom int64    `json:"time_from"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if len(params.IDs) > 0 && params.IDs[0] == "22" {
				result = []any{map[string]string{"eventid": "22", "objectid": "700", "source": "0", "object": "0", "value": "0", "clock": strconv.FormatInt(now.Unix(), 10), "ns": "0"}}
			} else if len(params.IDs) > 0 {
				recovery := "0"
				if mode.Load() == 2 {
					recovery = "22"
				}
				result = []any{problemEvent(recovery)}
			} else if params.From == "1" && params.TimeFrom < now.Add(-time.Hour).Unix() {
				result = []any{problemEvent("0")}
			}
		case "trigger.get":
			result = []any{map[string]any{"triggerid": "700", "description": "CPU high", "status": "0", "hosts": []any{map[string]string{"hostid": "101"}}}}
		default:
			t.Error("mutating or unsupported RPC", request.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: key}}}
	server.StartTLS()
	defer server.Close()
	policy, err := datasources.NewNetworkPolicy(datasources.NetworkConfig{AllowedOrigins: []string{server.URL}, AllowedCIDRs: []string{"127.0.0.0/8"}, AllowLoopback: true, CABundles: map[string][]byte{"lab": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := store.CollectorOperations().With(zabbix.New(policy).Operations()...)
	if err != nil {
		t.Fatal(err)
	}
	store.ConfigureDataSourceOperations(registry)
	vault, _ := datasources.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	if err = store.ConfigureDataSources(ctx, vault); err != nil {
		t.Fatal(err)
	}
	hash, err := access.HashPassword("fixture-backend-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AccessCreateUser(ctx, access.Principal{}, "zabbix-owner", "admin", hash, true, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	auth := access.New(store, access.Config{Mode: "required", SessionTTL: time.Hour})
	principal, err := auth.Login(ctx, "zabbix-owner", "fixture-backend-long-password", access.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rest := httptest.NewServer(api.New(store, logger, api.WithAccess(auth), api.WithAssets(store), api.WithDataSources(store), api.WithIncidents(store)))
	defer rest.Close()
	call := func(method, path string, body any, authenticated bool, key string, expected int) []byte {
		t.Helper()
		var encoded []byte
		if body != nil {
			encoded, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		request, err := http.NewRequestWithContext(ctx, method, rest.URL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if authenticated {
			request.Header.Set("Authorization", "Bearer "+principal.Token)
		}
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != expected {
			t.Fatal("REST response", method, path, response.StatusCode, string(raw), err)
		}
		if bytes.Contains(raw, []byte(token)) {
			t.Fatal("source token leaked through REST")
		}
		return raw
	}
	credential := token
	body := datasources.Input{Type: "ZABBIX", Name: "Zabbix lab", Endpoint: server.URL + "/api_jsonrpc.php", Enabled: true, SyncIntervalSeconds: 86400, Credential: &credential, Configuration: datasources.Settings{TimeoutSeconds: 5, TLSCARef: "lab", Zabbix: &datasources.ZabbixSettings{HostIDs: []string{"101"}, DeviceType: "switch", Monitoring: &datasources.ZabbixMonitoring{Enabled: true, ItemMappings: []externalmonitor.Mapping{{HostID: "101", ItemID: "901", Metric: "cpu.utilization", SourceUnit: "%", Unit: "percent"}}}}}}
	var source datasources.Source
	if json.Unmarshal(call("POST", "/api/data-sources", body, true, "", 201), &source) != nil {
		t.Fatal("source decode")
	}
	var job datasources.Job
	if json.Unmarshal(call("POST", "/api/data-sources/"+source.ID+"/sync", nil, true, "tls-sync-1", 202), &job) != nil {
		t.Fatal("job decode")
	}
	workerCtx, workerCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { datasources.Run(workerCtx, store, registry, logger); close(done) }()
	defer func() {
		workerCancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	wait := func(id string, want string) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			current, err := store.DataSourceJob(ctx, source.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			if current.State == want {
				return
			}
			if current.State == "failed" || current.State == "cancelled" {
				t.Fatal("job failed", current)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		t.Fatal("worker deadline")
	}
	wait(job.ID, "succeeded")
	var signals struct {
		Items []externalmonitor.SignalRecord `json:"items"`
	}
	raw := call("GET", "/api/signals?source_instance_id="+source.ID, nil, true, "", 200)
	if json.Unmarshal(raw, &signals) != nil || len(signals.Items) != 1 || len(signals.Items[0].AssetIDs) != 1 {
		t.Fatal("signal/asset mapping")
	}
	signal := signals.Items[0]
	asset := signal.AssetIDs[0]
	call("GET", "/api/signals", nil, false, "", 401)
	call("GET", "/api/assets/"+asset+"/external-metrics", nil, false, "", 401)
	var metrics struct {
		Items []externalmonitor.MetricRecord `json:"items"`
	}
	if json.Unmarshal(call("GET", "/api/assets/"+asset+"/external-metrics", nil, true, "", 200), &metrics) != nil || len(metrics.Items) != 1 || metrics.Items[0].Value == nil || *metrics.Items[0].Value != "82.125" {
		t.Fatal("metric REST")
	}
	call("POST", "/api/incidents/"+signal.IncidentID+"/acknowledge", map[string]any{"confirmed": true}, true, "", 200)
	mode.Store(2)
	if json.Unmarshal(call("POST", "/api/data-sources/"+source.ID+"/sync", nil, true, "tls-sync-2", 202), &job) != nil {
		t.Fatal("job decode")
	}
	wait(job.ID, "succeeded")
	var incident health.Incident
	if json.Unmarshal(call("GET", "/api/incidents/"+signal.IncidentID, nil, true, "", 200), &incident) != nil || incident.State != "resolved" || incident.AgentID != "" || incident.SourceInstanceID == nil {
		t.Fatal("common incident recovery REST", incident)
	}
	call("GET", "/api/data-sources/"+source.ID+"/signals/11/history", nil, true, "", 200)
	call("GET", "/api/data-sources/"+source.ID+"/metrics/history?item_id=901", nil, true, "", 200)
	call("GET", "/api/data-sources/"+source.ID+"/signals/11/history?limit=201", nil, true, "", 400)
}
