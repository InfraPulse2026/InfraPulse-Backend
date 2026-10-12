package health

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
)

type ResourcePolicy struct {
	UtilWarning   float64 `json:"utilization_warning_percent"`
	UtilCritical  float64 `json:"utilization_critical_percent"`
	UtilRecovery  float64 `json:"utilization_recovery_percent"`
	ErrorWarning  float64 `json:"errors_warning_per_second"`
	ErrorCritical float64 `json:"errors_critical_per_second"`
	ErrorRecovery float64 `json:"errors_recovery_per_second"`
	DropWarning   float64 `json:"drops_warning_per_second"`
	DropCritical  float64 `json:"drops_critical_per_second"`
	DropRecovery  float64 `json:"drops_recovery_per_second"`
	SIPWarning    float64 `json:"sip_latency_warning_ms"`
	SIPCritical   float64 `json:"sip_latency_critical_ms"`
	SIPRecovery   float64 `json:"sip_latency_recovery_ms"`
}

func ResourceDefaults() ResourcePolicy {
	return ResourcePolicy{80, 95, 70, 1, 10, 0.1, 10, 100, 1, 250, 1000, 150}
}

type ResourcePoint struct {
	RecordID   int64     `json:"record_id"`
	RecordKind string    `json:"record_kind"`
	ObservedAt time.Time `json:"observed_at"`
	Value      *float64  `json:"value"`
	State      string    `json:"state,omitempty"`
}
type ResourceSample struct {
	ID                     int64
	ObservedAt, ReceivedAt time.Time
	BootID                 string
	Identity               string // boot for interfaces; address/transport for SIP
	Interface              *pb.Interface
	SIP                    *pb.TelephonyHealth
}

func SIPContext(address, transport string) string {
	raw, _ := json.Marshal([]string{address, transport})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func resourceSeries(now time.Time, input []ResourceSample, interval int) []ResourceSample {
	samples := append([]ResourceSample(nil), input...)
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].ObservedAt.Equal(samples[j].ObservedAt) {
			return samples[i].ID < samples[j].ID
		}
		return samples[i].ObservedAt.Before(samples[j].ObservedAt)
	})
	clean := []ResourceSample{}
	for _, v := range samples {
		if v.ObservedAt.After(now) || v.ObservedAt.After(v.ReceivedAt.Add(30*time.Second)) {
			continue
		}
		if len(clean) > 0 && clean[len(clean)-1].ObservedAt.Equal(v.ObservedAt) {
			clean[len(clean)-1] = v
		} else {
			clean = append(clean, v)
		}
	}
	if len(clean) > 0 {
		start := len(clean) - 1
		gap := time.Duration(monitoring.StaleAfterSeconds(interval)) * time.Second
		for start > 0 && clean[start-1].BootID == clean[len(clean)-1].BootID && clean[start-1].Identity == clean[len(clean)-1].Identity && clean[start].ObservedAt.Sub(clean[start-1].ObservedAt) <= gap {
			start--
		}
		clean = clean[start:]
	}
	return clean
}
func resourceBase(now time.Time, s []ResourceSample, interval int, rule, metric, unit string) Assessment {
	p := ResourceDefaults()
	a := Assessment{RuleID: rule, RuleVersion: "1", Policy: Defaults(), Status: "unknown", Severity: "none", Reason: "no_data", Evidence: []Point{}, Metric: metric, Unit: unit, ResourcePolicy: &p}
	if len(s) == 0 {
		return a
	}
	at := s[len(s)-1].ObservedAt
	a.ObservedAt = &at
	a.Reason = "metric_missing_or_invalid"
	if now.Sub(at) > time.Duration(monitoring.StaleAfterSeconds(interval))*time.Second {
		a.Status = "stale"
		a.Reason = "observation_stale"
	}
	return a
}
func numberRule(now time.Time, s []ResourceSample, interval int, rule, metric, unit, kind string, warn, critical, recover float64, value func(ResourceSample) *float64) Assessment {
	a := resourceBase(now, s, interval, rule, metric, unit)
	if len(s) == 0 || a.Status == "stale" {
		return a
	}
	points := []ResourcePoint{}
	for i := len(s) - 1; i >= 0 && len(points) < 120; i-- {
		v := value(s[i])
		if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
			break
		}
		points = append([]ResourcePoint{{RecordID: s[i].ID, RecordKind: kind, ObservedAt: s[i].ObservedAt, Value: v}}, points...)
		if len(points) >= 3 && points[len(points)-1].ObservedAt.Sub(points[0].ObservedAt).Seconds() >= float64(max(6, 2*interval)) {
			break
		}
	}
	if len(points) == 0 {
		return a
	}
	a.ResourceEvidence = points
	a.NumericValue = points[len(points)-1].Value
	if unit == "percent" {
		a.Value = a.NumericValue
	}
	a.WindowSeconds = points[len(points)-1].ObservedAt.Sub(points[0].ObservedAt).Seconds()
	mature := len(points) >= 3 && a.WindowSeconds >= float64(max(6, 2*interval))
	high, crit, low := true, true, true
	for _, pt := range points {
		high = high && *pt.Value >= warn
		crit = crit && *pt.Value >= critical
		low = low && *pt.Value <= recover
	}
	a.Status = "ok"
	a.Reason = "below_threshold"
	a.RecoveryEligible = mature && low
	if mature && high {
		a.Status = "warning"
		a.Severity = "warning"
		a.Reason = "sustained_threshold"
		if crit {
			a.Status = "critical"
			a.Severity = "critical"
		}
	} else if *a.NumericValue >= warn {
		a.Status = "pending"
		a.Reason = "spike_not_sustained"
	}
	return a
}
func adminKnown(i *pb.Interface) bool {
	return i != nil && i.Metadata != nil && i.Metadata.AdminStatus && i.AdminUp
}
func rateAvailable(r *pb.InterfaceRates, name string) bool {
	if r == nil {
		return false
	}
	m := r.Available
	if m == nil {
		return true
	}
	switch name {
	case "traffic":
		return m.RxBytes && m.TxBytes
	case "errors":
		return m.RxErrors && m.TxErrors
	default:
		return m.RxDropped && m.TxDropped
	}
}
func interfaceValue(v ResourceSample, metric string) *float64 {
	i := v.Interface
	if !adminKnown(i) || !rateAvailable(i.Rates, metric) {
		return nil
	}
	r := i.Rates
	var n float64
	switch metric {
	case "traffic":
		if i.SpeedBitsPerSecond == nil || *i.SpeedBitsPerSecond == 0 || r.RxBytesPerSecond < 0 || r.TxBytesPerSecond < 0 {
			return nil
		}
		n = 8 * max(r.RxBytesPerSecond, r.TxBytesPerSecond) / float64(*i.SpeedBitsPerSecond) * 100
		if n > 100 {
			return nil
		}
	case "errors":
		if r.RxErrorsPerSecond < 0 || r.TxErrorsPerSecond < 0 {
			return nil
		}
		n = r.RxErrorsPerSecond + r.TxErrorsPerSecond
	default:
		if r.RxDroppedPerSecond < 0 || r.TxDroppedPerSecond < 0 {
			return nil
		}
		n = r.RxDroppedPerSecond + r.TxDroppedPerSecond
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func InterfaceRules(now time.Time, input []ResourceSample, interval int) []Assessment {
	s := resourceSeries(now, input, interval)
	p := ResourceDefaults()
	rules := []Assessment{numberRule(now, s, interval, "interface.oper_down", "oper_down", "boolean", "telemetry", 1, 1, 0, func(v ResourceSample) *float64 {
		if !adminKnown(v.Interface) {
			return nil
		}
		state := strings.ToLower(v.Interface.OperState)
		n := 0.0
		if state == "down" || state == "lowerlayerdown" {
			n = 1
		} else if state != "up" {
			return nil
		}
		return &n
	}), numberRule(now, s, interval, "interface.utilization", "utilization", "percent", "telemetry", p.UtilWarning, p.UtilCritical, p.UtilRecovery, func(v ResourceSample) *float64 { return interfaceValue(v, "traffic") }), numberRule(now, s, interval, "interface.errors", "errors", "per_second", "telemetry", p.ErrorWarning, p.ErrorCritical, p.ErrorRecovery, func(v ResourceSample) *float64 { return interfaceValue(v, "errors") }), numberRule(now, s, interval, "interface.drops", "drops", "per_second", "telemetry", p.DropWarning, p.DropCritical, p.DropRecovery, func(v ResourceSample) *float64 { return interfaceValue(v, "drops") })}
	if len(s) > 0 && !adminKnown(s[len(s)-1].Interface) {
		for i := range rules {
			rules[i].Reason = "admin_disabled_or_unknown"
		}
	}
	return rules
}
func sipGrade(h *pb.TelephonyHealth) *float64 {
	if h == nil {
		return nil
	}
	grade := 0.0
	switch h.State {
	case "down":
		if h.Reachable {
			return nil
		}
		grade = 2
	case "degraded":
		grade = 1
	case "up", "healthy":
		if !h.Reachable || h.ResponseCode == nil || *h.ResponseCode < 200 || *h.ResponseCode >= 300 {
			return nil
		}
	default:
		return nil
	}
	return &grade
}
func SIPRules(now time.Time, input []ResourceSample, interval int) []Assessment {
	s := resourceSeries(now, input, interval)
	p := ResourceDefaults()
	availability := numberRule(now, s, interval, "sip.availability", "reported_sip_health", "state_grade", "telephony", 1, 2, 0, func(v ResourceSample) *float64 { return sipGrade(v.SIP) })
	for i := range availability.ResourceEvidence {
		for _, v := range s {
			if v.ID == availability.ResourceEvidence[i].RecordID && v.SIP != nil {
				availability.ResourceEvidence[i].State = v.SIP.State
				break
			}
		}
	}
	latency := numberRule(now, s, interval, "sip.latency", "latency", "milliseconds", "telephony", p.SIPWarning, p.SIPCritical, p.SIPRecovery, func(v ResourceSample) *float64 {
		if v.SIP == nil || !v.SIP.Reachable {
			return nil
		}
		return v.SIP.LatencyMs
	})
	return []Assessment{availability, latency}
}
