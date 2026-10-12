package zabbix

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
)

type rpcRequest struct {
	JSONRPC string                     `json:"jsonrpc"`
	Method  string                     `json:"method"`
	ID      int                        `json:"id"`
	Auth    string                     `json:"auth"`
	Params  map[string]json.RawMessage `json:"params"`
}
type fakeTransport struct {
	t         *testing.T
	version   string
	legacy    bool
	hostReply func(rpcRequest) datasources.HTTPResponse
	calls     int
	closed    bool
}

func (f *fakeTransport) Close() { f.closed = true }
func (f *fakeTransport) Do(_ context.Context, r datasources.HTTPRequest) (datasources.HTTPResponse, error) {
	f.calls++
	var raw map[string]json.RawMessage
	if json.Unmarshal(r.Body, &raw) != nil || r.Method != "POST" || r.Path != "" || len(r.Query) != 0 {
		f.t.Fatal("invalid RPC transport request")
	}
	var req rpcRequest
	// apiinfo.version params are an array; authenticated calls use an object.
	if json.Unmarshal(raw["jsonrpc"], &req.JSONRPC) != nil || json.Unmarshal(raw["method"], &req.Method) != nil || json.Unmarshal(raw["id"], &req.ID) != nil || req.JSONRPC != "2.0" {
		f.t.Fatal("invalid JSON-RPC envelope")
	}
	_ = json.Unmarshal(raw["auth"], &req.Auth)
	if req.Method == "apiinfo.version" {
		if len(r.Bearer) != 0 || req.Auth != "" || raw["auth"] != nil || string(raw["params"]) != "[]" {
			f.t.Fatal("version probe leaked token")
		}
		return rpcResult(req.ID, f.version), nil
	}
	if req.Method != "host.get" {
		f.t.Fatal("write or unapproved API method called")
	}
	if json.Unmarshal(raw["params"], &req.Params) != nil {
		f.t.Fatal("bad host parameters")
	}
	if f.legacy {
		if req.Auth != "fixture-zabbix-token" || len(r.Bearer) != 0 {
			f.t.Fatal("6.0 authentication incorrect")
		}
	} else {
		if req.Auth != "" || raw["auth"] != nil || string(r.Bearer) != "fixture-zabbix-token" {
			f.t.Fatal("7.x authentication incorrect")
		}
	}
	if f.hostReply != nil {
		return f.hostReply(req), nil
	}
	return rpcResult(req.ID, []any{}), nil
}
func rpcResult(id int, value any) datasources.HTTPResponse {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": value})
	return datasources.HTTPResponse{StatusCode: 200, Body: raw}
}
func fixtureConnector(t *testing.T, version string) (*Connector, *fakeTransport, datasources.Request) {
	t.Helper()
	f := &fakeTransport{t: t, version: version, legacy: strings.HasPrefix(version, "6.")}
	c := &Connector{open: func(datasources.Source) (transport, error) { return f, nil }}
	return c, f, datasources.Request{Source: datasources.Source{ID: "src_fixture", Type: "ZABBIX", Enabled: true, Configuration: datasources.Settings{TimeoutSeconds: 15, Zabbix: &datasources.ZabbixSettings{HostIDs: []string{"3", "1", "2"}, PageSize: 2, DeviceType: "switch"}}}, Credential: []byte("fixture-zabbix-token")}
}
func fixtureHost(id string) map[string]any {
	return map[string]any{"hostid": id, "host": "switch-" + id, "name": "Switch " + id, "status": "0", "maintenance_status": "0", "maintenance_type": "0", "active_available": "0", "inventory": map[string]string{"vendor": "Eltex", "model": "MES2324B", "serialno_a": "SERIAL-" + id}, "interfaces": []any{map[string]string{"interfaceid": "10", "type": "2", "main": "1", "useip": "1", "ip": "192.0.2.42", "dns": "", "port": "161", "available": "2"}}, "tags": []any{map[string]string{"tag": "api_token", "value": "fixture-zabbix-token"}}}
}

func TestSupportedVersionsAuthenticationAndReadOnlyTest(t *testing.T) {
	for _, version := range []string{"6.0.40", "7.0.31", "7.4.15"} {
		t.Run(version, func(t *testing.T) {
			c, f, r := fixtureConnector(t, version)
			result, err := c.test(t.Context(), r)
			if err != nil || len(result.Observations) != 0 || len(result.Cursor) != 0 || f.calls != 2 || !f.closed {
				t.Fatal("test result", result, err, f.calls)
			}
		})
	}
	for _, version := range []string{"8.0.0", "6.4.1", "7.2.0", "7.4", "7.4.1rc1", "junk"} {
		t.Run(version, func(t *testing.T) {
			c, f, r := fixtureConnector(t, version)
			_, err := c.test(t.Context(), r)
			code, _ := datasources.Outcome(err)
			if code != "unsupported_version" || f.calls != 1 || !f.closed {
				t.Fatal("version not rejected before token", code, f.calls)
			}
		})
	}
}

func TestSelectedHostPagingNormalizationAndCursor(t *testing.T) {
	for _, version := range []string{"6.0.40", "7.0.31", "7.4.15"} {
		t.Run(version, func(t *testing.T) {
			c, f, r := fixtureConnector(t, version)
			var requested [][]string
			f.hostReply = func(req rpcRequest) datasources.HTTPResponse {
				var ids []string
				_ = json.Unmarshal(req.Params["hostids"], &ids)
				requested = append(requested, ids)
				if f.legacy {
					if req.Params["selectGroups"] == nil || req.Params["selectHostGroups"] != nil {
						t.Fatal("6.0 groups mapping")
					}
				} else {
					if req.Params["selectHostGroups"] == nil || req.Params["selectGroups"] != nil {
						t.Fatal("7.x groups mapping")
					}
				}
				if strings.Contains(string(req.Params["selectInterfaces"]), "details") || strings.Contains(string(req.Params["output"]), "extend") {
					t.Fatal("secret or unbounded property requested")
				}
				hosts := []any{}
				for i := len(ids) - 1; i >= 0; i-- {
					hosts = append(hosts, fixtureHost(ids[i]))
				}
				return rpcResult(req.ID, hosts)
			}
			for _, offset := range []int{2, 0, 2} {
				result, err := c.sync(t.Context(), r)
				if err != nil {
					t.Fatal(err)
				}
				var position cursor
				_ = json.Unmarshal(result.Cursor, &position)
				if position.HostOffset != offset {
					t.Fatal("cursor", position)
				}
				for _, o := range result.Observations {
					if !assets.ValidObservation(o) || o.Device.Online || assets.Kind(o.Device) != "network_device" || o.Device.GetObservation().GetSource().CollectorId != "" || o.Device.GetObservation().GetSource().InstanceId != r.Source.ID || o.Device.IdentityEvidence[0].Confidence != "medium" {
						t.Fatal("false identity/online/Agent", o)
					}
					raw, _ := json.Marshal(o)
					if strings.Contains(string(raw), "fixture-zabbix-token") {
						t.Fatal("echoed credential leaked")
					}
				}
				r.Cursor = result.Cursor
			}
			if !reflect.DeepEqual(requested, [][]string{{"1", "2"}, {"3"}, {"1", "2"}}) {
				t.Fatal("host selection/page order", requested)
			}
		})
	}
}

func TestMissingHostsNoCursorAndSafeFailures(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		reply      func(rpcRequest) datasources.HTTPResponse
	}{
		{"missing", "selected_hosts_unavailable", func(r rpcRequest) datasources.HTTPResponse { return rpcResult(r.ID, []any{fixtureHost("1")}) }},
		{"unexpected", "invalid_response", func(r rpcRequest) datasources.HTTPResponse {
			return rpcResult(r.ID, []any{fixtureHost("1"), fixtureHost("99")})
		}},
		{"duplicate", "invalid_response", func(r rpcRequest) datasources.HTTPResponse {
			return rpcResult(r.ID, []any{fixtureHost("1"), fixtureHost("1")})
		}},
		{"wrong_id", "invalid_response", func(r rpcRequest) datasources.HTTPResponse { return rpcResult(r.ID+1, []any{}) }},
		{"rpc_error", "api_rejected", func(r rpcRequest) datasources.HTTPResponse {
			return datasources.HTTPResponse{StatusCode: 200, Body: []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"fixture-zabbix-token","data":"secret"}}`, r.ID))}
		}},
		{"http_401", "invalid_credentials", func(r rpcRequest) datasources.HTTPResponse {
			return datasources.HTTPResponse{StatusCode: 401, Body: []byte("fixture-zabbix-token")}
		}},
		{"http_429", "rate_limited", func(r rpcRequest) datasources.HTTPResponse { return datasources.HTTPResponse{StatusCode: 429} }},
		{"http_503", "connection_failed", func(r rpcRequest) datasources.HTTPResponse { return datasources.HTTPResponse{StatusCode: 503} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, r := fixtureConnector(t, "7.4.1")
			f.hostReply = tc.reply
			result, err := c.sync(t.Context(), r)
			code, _ := datasources.Outcome(err)
			if code != tc.code || len(result.Cursor) != 0 || len(result.Observations) != 0 || strings.Contains(err.Error(), "fixture") {
				t.Fatal("unsafe/incomplete result", code, result, err)
			}
		})
	}
	c, f, r := fixtureConnector(t, "7.4.1")
	r.Source.Configuration.Zabbix = nil
	_, err := c.sync(t.Context(), r)
	code, _ := datasources.Outcome(err)
	if code != "configuration_required" || f.calls != 0 {
		t.Fatal("unconfigured source contacted")
	}
	r.Source.Configuration.Zabbix = &datasources.ZabbixSettings{HostIDs: []string{"1"}}
	r.Cursor = json.RawMessage(`{"host_offset":-1}`)
	_, err = c.sync(t.Context(), r)
	code, _ = datasources.Outcome(err)
	if code != "invalid_response" || f.calls != 0 {
		t.Fatal("corrupt cursor accepted")
	}
}
