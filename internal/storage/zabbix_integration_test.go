package storage

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/zabbix"
)

func zabbixTLSFixture(t *testing.T, handler http.HandlerFunc) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func TestZabbixTLSQueueInventoryHistoryCursorAndSourceIsolation(t *testing.T) {
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	const token = "fixture-zabbix-private-token"
	var status atomic.Int32
	var calls atomic.Int32
	server, ca := zabbixTLSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api_jsonrpc.php" || r.URL.RawQuery != "" {
			t.Error("wrong API destination")
		}
		var req struct {
			Method string          `json:"method"`
			ID     int             `json:"id"`
			Auth   string          `json:"auth"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("invalid RPC request")
			w.WriteHeader(400)
			return
		}
		var result any
		switch req.Method {
		case "apiinfo.version":
			if r.Header.Get("Authorization") != "" || req.Auth != "" {
				t.Error("version probe received token")
			}
			result = "7.4.15"
		case "host.get":
			if r.Header.Get("Authorization") != "Bearer "+token || req.Auth != "" {
				t.Error("token authentication incorrect")
			}
			var params struct {
				HostIDs []string `json:"hostids"`
				Limit   int      `json:"limit"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.Limit == 1 {
				result = []any{map[string]string{"hostid": "101"}}
				break
			}
			items := []any{}
			for _, id := range params.HostIDs {
				items = append(items, map[string]any{"hostid": id, "host": "eltex-" + id, "name": "Eltex switch " + id, "status": string(rune('0' + status.Load())), "maintenance_status": "0", "maintenance_type": "0", "active_available": "2", "inventory": map[string]string{"vendor": "Eltex", "model": "MES2324B", "serialno_a": "ELTEX-" + id}, "interfaces": []any{}, "hostgroups": []any{map[string]string{"groupid": "5", "name": "Switches"}}, "tags": []any{map[string]string{"tag": "token", "value": token}}})
			}
			result = items
		default:
			t.Error("write API method invoked")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	})
	policy, err := datasources.NewNetworkPolicy(datasources.NetworkConfig{AllowedOrigins: []string{server.URL}, AllowedCIDRs: []string{"127.0.0.0/8"}, AllowLoopback: true, CABundles: map[string][]byte{"fixture": ca}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := s.CollectorOperations().With(zabbix.New(policy).Operations()...)
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureDataSourceOperations(registry)
	vault, err := datasources.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureDataSources(ctx, vault); err != nil {
		t.Fatal(err)
	}
	credential := token
	in := datasources.Input{Type: "ZABBIX", Name: "Zabbix production", Endpoint: server.URL + "/api_jsonrpc.php", Enabled: true, SyncIntervalSeconds: 60, Credential: &credential, Configuration: datasources.Settings{TimeoutSeconds: 5, TLSCARef: "fixture", Zabbix: &datasources.ZabbixSettings{HostIDs: []string{"101", "102"}, PageSize: 1, DeviceType: "switch"}}}
	source, err := s.SaveDataSource(ctx, "", in)
	if err != nil {
		t.Fatal(err)
	}
	run := func(sourceID, operation, key string) datasources.Result {
		t.Helper()
		if _, _, err := s.QueueDataSourceJob(ctx, sourceID, operation, key); err != nil {
			t.Fatal(err)
		}
		request := claimSourceJob(t, s, ctx)
		op, ok := registry.Lookup(request.Source.Type, operation)
		if !ok {
			t.Fatal("adapter not registered")
		}
		result, runErr := op.Run(ctx, *request)
		clear(request.Credential)
		request.Credential = nil
		if runErr != nil {
			t.Fatal(runErr)
		}
		if err = s.FinishDataSourceJob(ctx, *request, result, nil); err != nil {
			t.Fatal(err)
		}
		return result
	}
	run(source.ID, "test", "test-first")
	public, err := s.DataSource(ctx, source.ID)
	if err != nil || public.ConnectionStatus != "connected" || public.LastSuccessfulSync != nil {
		t.Fatal("test falsely marked inventory synchronized", public, err)
	}
	first := run(source.ID, "sync", "sync-page-1")
	if !strings.Contains(string(first.Cursor), `"host_offset":1`) {
		t.Fatal("cursor not advanced", string(first.Cursor))
	}
	run(source.ID, "sync", "sync-page-2")
	items, err := s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(items) != 2 {
		t.Fatal("Zabbix assets missing", items, err)
	}
	var firstAsset string
	for _, a := range items {
		bindings, err := s.AssetBindings(ctx, a.ID, true, 50, 0)
		if err != nil || len(bindings) != 1 {
			t.Fatal(err)
		}
		b := bindings[0]
		if b.SourceType != "ZABBIX" || b.LegacyAgentID != nil || b.LegacyDeviceID != nil || b.Provenance != "external" || b.ExternalType != "host" {
			t.Fatal("invented Agent or lost source", b)
		}
		if b.ExternalID == "101" {
			firstAsset = a.ID
		}
		raw, _ := json.Marshal(b)
		if bytes.Contains(raw, []byte(token)) || !bytes.Contains(raw, []byte("[REDACTED]")) {
			t.Fatal("credential persisted in public facts")
		}
	}
	if firstAsset == "" {
		t.Fatal("host 101 missing")
	}
	run(source.ID, "sync", "repeat-unchanged")
	facts, err := s.AssetFacts(ctx, firstAsset, 50, 0)
	if err != nil || len(facts) != 1 {
		t.Fatal("unchanged data grew history", len(facts), err)
	}
	status.Store(1)
	run(source.ID, "sync", "other-host-disabled")
	run(source.ID, "sync", "host-disabled")
	facts, err = s.AssetFacts(ctx, firstAsset, 50, 0)
	if err != nil || len(facts) != 2 || !bytes.Contains(facts[0].ProviderState, []byte(`"monitoring_enabled": false`)) {
		t.Fatal("source state history lost", facts, err)
	}
	in.Name = "Zabbix second instance"
	other, err := s.SaveDataSource(ctx, "", in)
	if err != nil {
		t.Fatal(err)
	}
	run(other.ID, "sync", "second-instance")
	items, err = s.Assets(ctx, "network_device", "real", 50, 0)
	if err != nil || len(items) != 3 {
		t.Fatal("same host ID crossed source instance", items, err)
	}
	// A completed HTTP read cannot publish after a settings revision changes.
	if _, _, err = s.QueueDataSourceJob(ctx, source.ID, "sync", "stale-revision"); err != nil {
		t.Fatal(err)
	}
	request := claimSourceJob(t, s, ctx)
	op, _ := registry.Lookup("ZABBIX", "sync")
	result, err := op.Run(ctx, *request)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.AssetFacts(ctx, firstAsset, 50, 0)
	in.Revision = source.Revision
	in.Credential = nil
	in.Name = "Zabbix updated"
	in.Configuration.Zabbix.DeviceType = "server"
	if _, err = s.SaveDataSource(ctx, source.ID, in); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishDataSourceJob(ctx, *request, result, nil); err != nil {
		t.Fatal(err)
	}
	job, err := s.DataSourceJob(ctx, source.ID, request.Job.ID)
	if err != nil || job.State != "cancelled" || job.Code != "configuration_changed" {
		t.Fatal("stale response not cancelled", job, err)
	}
	clear(request.Credential)
	after, _ := s.AssetFacts(ctx, firstAsset, 50, 0)
	if len(after) != len(before) {
		t.Fatal("stale result altered inventory")
	}
	var cursorValue string
	if err = s.pool.QueryRow(ctx, `SELECT sync_cursor::text FROM data_sources WHERE id=$1`, source.ID).Scan(&cursorValue); err != nil || cursorValue != "{}" {
		t.Fatal("settings edit retained stale cursor", cursorValue, err)
	}
	if calls.Load() < 14 {
		t.Fatal("did not exercise real TLS/API reads", calls.Load())
	}
	var agentCount int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM agents`).Scan(&agentCount); err != nil || agentCount != 0 {
		t.Fatal("external inventory invented agents")
	}
}
