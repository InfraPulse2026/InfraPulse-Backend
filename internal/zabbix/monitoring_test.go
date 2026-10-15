package zabbix

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
)

type monTransport struct {
	t       *testing.T
	version string
	reply   func(rpcRequest) any
	status  int
	calls   []string
}

func (f *monTransport) Close() {}
func (f *monTransport) Do(_ context.Context, r datasources.HTTPRequest) (datasources.HTTPResponse, error) {
	var req rpcRequest
	if json.Unmarshal(r.Body, &req) != nil { // apiinfo params are []
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(r.Body, &raw)
		_ = json.Unmarshal(raw["method"], &req.Method)
		_ = json.Unmarshal(raw["id"], &req.ID)
	}
	f.calls = append(f.calls, req.Method)
	if req.Method == "apiinfo.version" {
		if len(r.Bearer) != 0 || req.Auth != "" {
			f.t.Fatal("version leaked credential")
		}
		return rpcResult(req.ID, f.version), nil
	}
	if strings.HasPrefix(f.version, "6.") {
		if req.Auth != "fixture-zabbix-token" || len(r.Bearer) > 0 {
			f.t.Fatal("6.x credential")
		}
	} else {
		if req.Auth != "" || string(r.Bearer) != "fixture-zabbix-token" {
			f.t.Fatal("7.x credential")
		}
	}
	if req.Method == "host.get" {
		var ids []string
		_ = json.Unmarshal(req.Params["hostids"], &ids)
		hosts := []any{}
		for _, id := range ids {
			hosts = append(hosts, fixtureHost(id))
		}
		return rpcResult(req.ID, hosts), nil
	}
	if f.status != 0 {
		return datasources.HTTPResponse{StatusCode: f.status}, nil
	}
	return rpcResult(req.ID, f.reply(req)), nil
}
func monitorFixture(t *testing.T, version string) (*Connector, *monTransport, datasources.Request) {
	t.Helper()
	f := &monTransport{t: t, version: version}
	c := &Connector{open: func(datasources.Source) (transport, error) { return f, nil }}
	r := datasources.Request{Source: datasources.Source{ID: "src_monitor", Type: "ZABBIX", Enabled: true, Configuration: datasources.Settings{TimeoutSeconds: 15, Zabbix: &datasources.ZabbixSettings{HostIDs: []string{"1"}, PageSize: 20, DeviceType: "server", Monitoring: &datasources.ZabbixMonitoring{Enabled: true, EventPageSize: 1, EventLookbackSeconds: 86400, ItemMappings: []externalmonitor.Mapping{{HostID: "1", ItemID: "901", Metric: "cpu.utilization", SourceUnit: "%", Unit: "percent", Multiplier: "1", MaxAgeSeconds: 300}}}}}}, Credential: []byte("fixture-zabbix-token")}
	return c, f, r
}
func monEvent(id, recovery string, now time.Time) map[string]string {
	return map[string]string{"eventid": id, "objectid": "700", "source": "0", "object": "0", "clock": strconv.FormatInt(now.Add(-time.Hour).Unix(), 10), "ns": "42", "value": "1", "name": "CPU high token=fixture-zabbix-token", "severity": "4", "acknowledged": "0", "suppressed": "0", "r_eventid": recovery}
}

func TestMonitoringMetricsActiveHistoryAndExplicitRecovery(t *testing.T) {
	for _, version := range []string{"6.0.40", "7.0.31", "7.4.15"} {
		t.Run(version, func(t *testing.T) {
			c, f, r := monitorFixture(t, version)
			now := time.Now()
			closed := false
			f.reply = func(req rpcRequest) any {
				switch req.Method {
				case "item.get":
					if strings.Contains(string(req.Params["output"]), "extend") {
						t.Fatal("all item properties requested")
					}
					return []any{map[string]string{"itemid": "901", "hostid": "1", "name": "CPU", "units": "%", "value_type": "0", "status": "0", "state": "0", "lastclock": strconv.FormatInt(now.Unix(), 10), "lastns": "100", "lastvalue": "82.125"}}
				case "problem.get":
					if string(req.Params["recent"]) != "false" {
						t.Fatal("active query changed")
					}
					if closed {
						return []any{}
					}
					return []any{monEvent("11", "0", now)}
				case "event.get":
					var ids []string
					_ = json.Unmarshal(req.Params["eventids"], &ids)
					if len(ids) > 0 && ids[0] == "22" {
						return []any{map[string]string{"eventid": "22", "objectid": "700", "source": "0", "object": "0", "value": "0", "clock": strconv.FormatInt(now.Unix(), 10), "ns": "0"}}
					}
					if len(ids) > 0 {
						return []any{monEvent("11", "22", now)}
					}
					var after string
					_ = json.Unmarshal(req.Params["eventid_from"], &after)
					if after != "1" {
						return []any{}
					}
					return []any{monEvent("11", "0", now)}
				case "trigger.get":
					return []any{map[string]any{"triggerid": "700", "description": "CPU high", "status": "0", "hosts": []any{map[string]string{"hostid": "1"}}}}
				default:
					t.Fatal("write/unknown method", req.Method)
					return nil
				}
			}
			first, err := c.sync(t.Context(), r)
			if err != nil || first.Monitoring == nil || len(first.Monitoring.Measurements) != 1 || len(first.Monitoring.Signals) != 1 {
				t.Fatal("monitoring missing", first, err)
			}
			metric := first.Monitoring.Measurements[0]
			if metric.Value == nil || *metric.Value != "82.125" || metric.Nanoseconds != 100 || metric.Quality != "fresh" {
				t.Fatal(metric)
			}
			if first.Monitoring.Signals[0].State != "active" || strings.Contains(first.Monitoring.Signals[0].Name, "fixture-zabbix-token") {
				t.Fatal("signal state/redaction")
			}
			r.Cursor = first.Cursor
			r.PendingSignals = []externalmonitor.Pending{{EventID: "11", TriggerID: "700", HostIDs: []string{"1"}}}
			closed = true
			second, err := c.sync(t.Context(), r)
			if err != nil || len(second.Monitoring.Signals) != 1 || second.Monitoring.Signals[0].State != "resolved" || second.Monitoring.Signals[0].RecoveryEventID != "22" || second.Monitoring.Signals[0].RecoveredAt == nil {
				t.Fatal("explicit recovery not followed", second, err)
			}
			f.status = 503
			result, err := c.sync(t.Context(), r)
			code, _ := datasources.Outcome(err)
			if code != "connection_failed" || result.Monitoring != nil || len(result.Cursor) != 0 || len(result.Observations) != 0 {
				t.Fatal("partial data on API failure", code, result)
			}
		})
	}
}

func TestMonitoringMissingItemAndCrossHostInjection(t *testing.T) {
	c, f, r := monitorFixture(t, "7.4.15")
	f.reply = func(req rpcRequest) any { return []any{} }
	result, err := c.sync(t.Context(), r)
	if err != nil || len(result.Monitoring.Measurements) != 1 || result.Monitoring.Measurements[0].Quality != "unavailable" || result.Monitoring.Measurements[0].Value != nil || len(result.Monitoring.Signals) != 0 {
		t.Fatal("missing data invented zero", result, err)
	}
	f.reply = func(req rpcRequest) any {
		if req.Method == "item.get" {
			return []any{map[string]string{"itemid": "901", "hostid": "999", "status": "0", "state": "0"}}
		}
		return []any{}
	}
	result, err = c.sync(t.Context(), r)
	code, _ := datasources.Outcome(err)
	if code != "invalid_response" || result.Monitoring != nil {
		t.Fatal("cross host item accepted", code)
	}
}
