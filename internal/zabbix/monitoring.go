package zabbix

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
)

type eventWindow struct {
	After string `json:"after"`
	From  int64  `json:"from"`
}
type item struct {
	ID        string `json:"itemid"`
	HostID    string `json:"hostid"`
	Name      string `json:"name"`
	Units     string `json:"units"`
	ValueType string `json:"value_type"`
	Status    string `json:"status"`
	State     string `json:"state"`
	LastClock string `json:"lastclock"`
	LastNS    string `json:"lastns"`
	LastValue string `json:"lastvalue"`
}
type event struct {
	ID            string `json:"eventid"`
	TriggerID     string `json:"objectid"`
	Source        string `json:"source"`
	Object        string `json:"object"`
	Clock         string `json:"clock"`
	NS            string `json:"ns"`
	Value         string `json:"value"`
	Name          string `json:"name"`
	Severity      string `json:"severity"`
	Acknowledged  string `json:"acknowledged"`
	Suppressed    string `json:"suppressed"`
	RecoveryID    string `json:"r_eventid"`
	RecoveryClock string `json:"r_clock"`
	RecoveryNS    string `json:"r_ns"`
}
type trigger struct {
	ID          string `json:"triggerid"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Hosts       []struct {
		ID string `json:"hostid"`
	} `json:"hosts"`
}

func nextID(value string) (string, error) {
	if value == "" || value == "0" {
		return "1", nil
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n == ^uint64(0) || !canonicalID(value) {
		return "", datasources.Failure{Code: "invalid_response"}
	}
	return strconv.FormatUint(n+1, 10), nil
}
func numericSort(values []string) {
	sort.Slice(values, func(i, j int) bool {
		a, _ := strconv.ParseUint(values[i], 10, 64)
		b, _ := strconv.ParseUint(values[j], 10, 64)
		return a < b
	})
}
func remoteTime(clock, ns string, now time.Time) (time.Time, int, error) {
	n, err := strconv.ParseInt(clock, 10, 64)
	if err != nil || n <= 0 || n > 253402300799 {
		return time.Time{}, 0, datasources.Failure{Code: "invalid_response"}
	}
	nanos := 0
	if ns != "" {
		nanos, err = strconv.Atoi(ns)
	}
	if err != nil || nanos < 0 || nanos > 999999999 {
		return time.Time{}, 0, datasources.Failure{Code: "invalid_response"}
	}
	at := time.Unix(n, int64(nanos)).UTC()
	if at.After(now.Add(30 * time.Second)) {
		return time.Time{}, 0, datasources.Failure{Code: "invalid_response"}
	}
	return at, nanos, nil
}
func (c *client) safeText(value string, max int) (string, error) {
	v, err := clean(value, max)
	if len(c.token) > 0 {
		v = strings.ReplaceAll(v, string(c.token), "[REDACTED]")
	}
	return v, err
}

func (c *client) readItems(ctx context.Context, settings datasources.ZabbixSettings, selected []string, now time.Time) ([]externalmonitor.Measurement, error) {
	allowed := map[string]bool{}
	for _, id := range selected {
		allowed[id] = true
	}
	mappings := map[string]externalmonitor.Mapping{}
	ids := []string{}
	for _, m := range settings.Monitoring.ItemMappings {
		if allowed[m.HostID] {
			ids = append(ids, m.ItemID)
			mappings[m.ItemID] = m
		}
	}
	results := []externalmonitor.Measurement{}
	if len(ids) == 0 {
		return results, nil
	}
	var values []item
	err := c.call(ctx, "item.get", map[string]any{"itemids": ids, "hostids": selected, "output": []string{"itemid", "hostid", "name", "units", "value_type", "status", "state", "lastclock", "lastns", "lastvalue"}}, true, &values)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, v := range values {
		m, ok := mappings[v.ID]
		if !ok || v.HostID != m.HostID || seen[v.ID] || !oneOf(v.Status, "0", "1") || !oneOf(v.State, "0", "1") {
			return nil, datasources.Failure{Code: "invalid_response"}
		}
		seen[v.ID] = true
		name, err := c.safeText(v.Name, 256)
		if err != nil {
			return nil, err
		}
		unit, err := c.safeText(v.Units, 32)
		if err != nil {
			return nil, err
		}
		r := externalmonitor.Measurement{HostID: m.HostID, ItemID: m.ItemID, Name: name, Metric: m.Metric, Unit: m.Unit, SourceUnit: unit, Multiplier: m.Multiplier, MaxAgeSeconds: m.MaxAgeSeconds, Quality: "fresh"}
		if _, ok := externalmonitor.Number(v.LastValue); ok {
			r.RawValue = v.LastValue
		}
		switch {
		case v.Status == "1":
			r.Quality = "disabled"
		case v.State == "1":
			r.Quality = "unsupported"
		case !oneOf(v.ValueType, "0", "3"):
			r.Quality = "non_numeric"
		case unit != m.SourceUnit:
			r.Quality = "unit_mismatch"
		case v.LastClock == "" || v.LastClock == "0":
			r.Quality = "no_data"
		default:
			at, ns, timeErr := remoteTime(v.LastClock, v.LastNS, now)
			if timeErr != nil {
				r.Quality = "clock_skew"
				break
			}
			r.ObservedAt = &at
			r.Nanoseconds = ns
			if v.ValueType == "3" {
				if _, err := strconv.ParseUint(v.LastValue, 10, 64); err != nil {
					r.Quality = "invalid_value"
					break
				}
			}
			normal, ok := externalmonitor.NormalizeValue(v.LastValue, m)
			if !ok || len(normal) > 256 {
				r.Quality = "invalid_value"
				break
			}
			r.Value = &normal
			if now.Sub(at) > time.Duration(m.MaxAgeSeconds)*time.Second {
				r.Quality = "stale"
			}
		}
		results = append(results, r)
	}
	// A missing/unreadable selected item is explicit unavailable data, never zero.
	for _, id := range ids {
		if !seen[id] {
			m := mappings[id]
			results = append(results, externalmonitor.Measurement{HostID: m.HostID, ItemID: id, Metric: m.Metric, Unit: m.Unit, SourceUnit: m.SourceUnit, Multiplier: m.Multiplier, MaxAgeSeconds: m.MaxAgeSeconds, Quality: "unavailable"})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].ItemID < results[j].ItemID })
	return results, nil
}

var eventFields = []string{"eventid", "objectid", "source", "object", "clock", "ns", "value", "name", "severity", "acknowledged", "suppressed", "r_eventid"}
var problemFields = []string{"eventid", "objectid", "source", "object", "clock", "ns", "name", "severity", "acknowledged", "suppressed", "r_eventid", "r_clock", "r_ns"}

func (c *client) monitor(ctx context.Context, request datasources.Request, settings datasources.ZabbixSettings, selected []string, position *cursor, now time.Time) (*externalmonitor.Batch, error) {
	m := settings.Monitoring
	fail := datasources.Failure{Code: "invalid_response"}
	if position.HistoryFrom == 0 {
		position.HistoryFrom = now.Unix() - int64(m.EventLookbackSeconds)
	}
	if position.HistoryFrom < 1 || position.HistoryFrom > now.Unix() || len(position.EventWindows) > 200 || len(position.ActiveAfter) > 200 || len(request.PendingSignals) > 100 {
		return nil, fail
	}
	if position.EventWindows == nil {
		position.EventWindows = map[string]eventWindow{}
	}
	if position.ActiveAfter == nil {
		position.ActiveAfter = map[string]string{}
	}
	measurements, err := c.readItems(ctx, settings, selected, now)
	if err != nil {
		return nil, err
	}
	batch := &externalmonitor.Batch{Measurements: measurements, Signals: []externalmonitor.Signal{}}
	all := map[string]event{}
	hosts := map[string]map[string]bool{}
	add := func(e event, hostIDs []string) error {
		if !canonicalID(e.ID) || !canonicalID(e.TriggerID) || e.Source != "0" || e.Object != "0" || !oneOf(e.Value, "", "1") {
			return fail
		}
		if previous, ok := all[e.ID]; ok && previous.TriggerID != e.TriggerID {
			return fail
		}
		// Explicit recovery is monotonic even if an active poll races history.
		if previous, ok := all[e.ID]; !ok || previous.RecoveryID == "" || previous.RecoveryID == "0" {
			all[e.ID] = e
		}
		if hosts[e.ID] == nil {
			hosts[e.ID] = map[string]bool{}
		}
		for _, id := range hostIDs {
			hosts[e.ID][id] = true
		}
		return nil
	}
	for _, hostID := range selected {
		after, err := nextID(position.ActiveAfter[hostID])
		if err != nil {
			return nil, err
		}
		var problems []event
		if err = c.call(ctx, "problem.get", map[string]any{"hostids": []string{hostID}, "source": 0, "object": 0, "recent": false, "eventid_from": after, "output": problemFields, "sortfield": []string{"eventid"}, "sortorder": "ASC", "limit": m.EventPageSize}, true, &problems); err != nil {
			return nil, err
		}
		if len(problems) > m.EventPageSize {
			return nil, fail
		}
		last := uint64(0)
		for _, e := range problems {
			n, _ := strconv.ParseUint(e.ID, 10, 64)
			from, _ := strconv.ParseUint(after, 10, 64)
			if n < from || n <= last {
				return nil, fail
			}
			last = n
			if err = add(e, []string{hostID}); err != nil {
				return nil, err
			}
		}
		position.ActiveAfter[hostID] = "0"
		if len(problems) == m.EventPageSize {
			position.ActiveAfter[hostID] = strconv.FormatUint(last, 10)
		}
		window, ok := position.EventWindows[hostID]
		if !ok {
			window = eventWindow{From: position.HistoryFrom}
		}
		if window.From < position.HistoryFrom || window.From > now.Unix() {
			return nil, fail
		}
		after, err = nextID(window.After)
		if err != nil {
			return nil, err
		}
		var events []event
		if err = c.call(ctx, "event.get", map[string]any{"hostids": []string{hostID}, "source": 0, "object": 0, "value": 1, "time_from": window.From, "eventid_from": after, "output": eventFields, "sortfield": []string{"eventid"}, "sortorder": "ASC", "limit": m.EventPageSize}, true, &events); err != nil {
			return nil, err
		}
		if len(events) > m.EventPageSize {
			return nil, fail
		}
		last = 0
		for _, e := range events {
			n, _ := strconv.ParseUint(e.ID, 10, 64)
			from, _ := strconv.ParseUint(after, 10, 64)
			if n < from || n <= last {
				return nil, fail
			}
			last = n
			if err = add(e, []string{hostID}); err != nil {
				return nil, err
			}
		}
		if len(events) == m.EventPageSize {
			window.After = strconv.FormatUint(last, 10)
		} else {
			window.After = "0"
			window.From = max(position.HistoryFrom, now.Unix()-300)
		}
		position.EventWindows[hostID] = window
	}
	if len(request.PendingSignals) > 0 {
		ids := []string{}
		expected := map[string]externalmonitor.Pending{}
		for _, p := range request.PendingSignals {
			if !canonicalID(p.EventID) || !canonicalID(p.TriggerID) {
				return nil, fail
			}
			ids = append(ids, p.EventID)
			expected[p.EventID] = p
		}
		var pending []event
		if err = c.call(ctx, "event.get", map[string]any{"eventids": ids, "source": 0, "object": 0, "output": eventFields}, true, &pending); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, e := range pending {
			p, ok := expected[e.ID]
			if !ok || seen[e.ID] || e.TriggerID != p.TriggerID {
				return nil, fail
			}
			seen[e.ID] = true
			if err = add(e, p.HostIDs); err != nil {
				return nil, err
			}
		}
		// Missing historical events never imply resolution. Their last_seen stays old.
		position.ReconcileAfter = "0"
		if len(request.PendingSignals) == 100 {
			position.ReconcileAfter = request.PendingSignals[len(request.PendingSignals)-1].EventID
		}
	} else {
		position.ReconcileAfter = "0"
	}
	if len(all) > 1200 {
		return nil, datasources.Failure{Code: "response_too_large"}
	}
	triggerIDs := []string{}
	triggerSet := map[string]bool{}
	recoveryIDs := []string{}
	recoverySet := map[string]bool{}
	for _, e := range all {
		if !triggerSet[e.TriggerID] {
			triggerSet[e.TriggerID] = true
			triggerIDs = append(triggerIDs, e.TriggerID)
		}
		if e.RecoveryID != "" && e.RecoveryID != "0" && e.RecoveryClock == "" {
			if !canonicalID(e.RecoveryID) {
				return nil, fail
			}
			if !recoverySet[e.RecoveryID] {
				recoverySet[e.RecoveryID] = true
				recoveryIDs = append(recoveryIDs, e.RecoveryID)
			}
		}
	}
	triggers := map[string]trigger{}
	recoveries := map[string]event{}
	for start := 0; start < len(triggerIDs); start += 100 {
		var values []trigger
		if err = c.call(ctx, "trigger.get", map[string]any{"triggerids": triggerIDs[start:min(start+100, len(triggerIDs))], "output": []string{"triggerid", "description", "status"}, "selectHosts": []string{"hostid"}}, true, &values); err != nil {
			return nil, err
		}
		for _, v := range values {
			if !triggerSet[v.ID] || triggers[v.ID].ID != "" || !oneOf(v.Status, "0", "1") || len(v.Hosts) > 200 {
				return nil, fail
			}
			triggers[v.ID] = v
		}
	}
	for start := 0; start < len(recoveryIDs); start += 100 {
		var values []event
		if err = c.call(ctx, "event.get", map[string]any{"eventids": recoveryIDs[start:min(start+100, len(recoveryIDs))], "source": 0, "object": 0, "output": []string{"eventid", "objectid", "source", "object", "clock", "ns", "value"}}, true, &values); err != nil {
			return nil, err
		}
		for _, v := range values {
			if !recoverySet[v.ID] || recoveries[v.ID].ID != "" || v.Source != "0" || v.Object != "0" || v.Value != "0" {
				return nil, fail
			}
			recoveries[v.ID] = v
		}
	}
	ids := []string{}
	for id := range all {
		ids = append(ids, id)
	}
	numericSort(ids)
	for _, id := range ids {
		e := all[id]
		at, ns, err := remoteTime(e.Clock, e.NS, now)
		if err != nil {
			return nil, err
		}
		severity, err := strconv.Atoi(e.Severity)
		if err != nil || severity < 0 || severity > 5 || !oneOf(e.Acknowledged, "0", "1") || !oneOf(e.Suppressed, "", "0", "1") {
			return nil, fail
		}
		name, err := c.safeText(e.Name, 1024)
		if err != nil {
			return nil, err
		}
		hostIDs := []string{}
		for host := range hosts[id] {
			hostIDs = append(hostIDs, host)
		}
		numericSort(hostIDs)
		signal := externalmonitor.Signal{EventID: id, TriggerID: e.TriggerID, HostIDs: hostIDs, Name: name, Severity: severity, Acknowledged: e.Acknowledged == "1", Suppressed: e.Suppressed == "1", ObservedAt: at, Nanoseconds: ns, State: "active", MappingStatus: "host_filter"}
		if tr, ok := triggers[e.TriggerID]; ok {
			signal.TriggerName, err = c.safeText(tr.Description, 1024)
			if err != nil {
				return nil, err
			}
			signal.TriggerStatus = tr.Status
		} else {
			signal.MappingStatus = "trigger_unavailable"
		}
		if e.RecoveryID != "" && e.RecoveryID != "0" {
			clock, ns := e.RecoveryClock, e.RecoveryNS
			if clock == "" {
				recovery, ok := recoveries[e.RecoveryID]
				if !ok || recovery.TriggerID != e.TriggerID {
					return nil, fail
				}
				clock, ns = recovery.Clock, recovery.NS
			}
			closed, _, err := remoteTime(clock, ns, now)
			if err != nil || closed.Before(at) {
				return nil, fail
			}
			signal.State = "resolved"
			signal.RecoveryEventID = e.RecoveryID
			signal.RecoveredAt = &closed
		}
		batch.Signals = append(batch.Signals, signal)
	}
	return batch, nil
}
