// Package health evaluates bounded, source-isolated observations. It never
// predicts an actual outage or executes device commands.
package health

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
)

type Policy struct {
	CPUWarning             float64 `json:"cpu_warning_percent"`
	CPUCritical            float64 `json:"cpu_critical_percent"`
	CPURecovery            float64 `json:"cpu_recovery_percent"`
	RAMWarning             float64 `json:"ram_warning_percent"`
	RAMCritical            float64 `json:"ram_critical_percent"`
	RAMRecovery            float64 `json:"ram_recovery_percent"`
	ForecastHorizonSeconds int     `json:"forecast_horizon_seconds"`
}

func Defaults() Policy { return Policy{80, 95, 70, 80, 95, 75, 300} }
func (p Policy) Validate() error {
	for _, v := range [][3]float64{{p.CPURecovery, p.CPUWarning, p.CPUCritical}, {p.RAMRecovery, p.RAMWarning, p.RAMCritical}} {
		if math.IsNaN(v[0]) || math.IsNaN(v[1]) || math.IsNaN(v[2]) || v[0] < 0 || v[2] > 100 || v[0] >= v[1] || v[1] >= v[2] {
			return errors.New("health thresholds require 0 <= recovery < warning < critical <= 100")
		}
	}
	if p.ForecastHorizonSeconds < 60 || p.ForecastHorizonSeconds > 3600 {
		return errors.New("forecast horizon must be 60..3600 seconds")
	}
	return nil
}

type Sample struct {
	ID                     int64
	ObservedAt, ReceivedAt time.Time
	BootID                 string
	CPU, RAM               *float64
}
type Point struct {
	TelemetryID int64     `json:"telemetry_id"`
	ObservedAt  time.Time `json:"observed_at"`
	Value       float64   `json:"value_percent"`
}
type Assessment struct {
	Topology            *TopologyEvidence       `json:"topology,omitempty"`
	Configuration       *ConfigurationEvidence  `json:"configuration,omitempty"`
	Log                 *LogEvidence            `json:"log,omitempty"`
	Hardware            *HardwareEvidence       `json:"hardware,omitempty"`
	External            *externalmonitor.Signal `json:"external,omitempty"`
	SourceRevision      int64                   `json:"source_revision,omitempty"`
	FlapWindowSeconds   int                     `json:"flap_window_seconds,omitempty"`
	FlapRecoverySeconds int                     `json:"flap_recovery_seconds,omitempty"`
	LastStateChangeAt   *time.Time              `json:"last_state_change_at,omitempty"`
	Collector           *CollectorEvidence      `json:"collector,omitempty"`
	Metric              string                  `json:"metric,omitempty"`
	Unit                string                  `json:"unit,omitempty"`
	NumericValue        *float64                `json:"value,omitempty"`
	ResourceEvidence    []ResourcePoint         `json:"resource_evidence,omitempty"`
	ResourcePolicy      *ResourcePolicy         `json:"resource_policy,omitempty"`
	RuleVersion         string                  `json:"rule_version"`
	Policy              Policy                  `json:"policy"`
	RuleID              string                  `json:"rule_id"`
	Status              string                  `json:"status"`
	Severity            string                  `json:"severity"`
	Reason              string                  `json:"reason"`
	ObservedAt          *time.Time              `json:"observed_at"`
	Value               *float64                `json:"value_percent"`
	RecoveryEligible    bool                    `json:"recovery_eligible"`
	Evidence            []Point                 `json:"evidence"`
	WindowSeconds       float64                 `json:"window_seconds"`
	Slope               *float64                `json:"slope_percent_per_second"`
	Fit                 *float64                `json:"r_squared"`
	SecondsToCritical   *float64                `json:"seconds_to_critical"`
}

type HardwareEvidence struct {
	ClockQuality   string `json:"clock_quality"`
	SampleID       int64  `json:"sample_id"`
	ObservationKey string `json:"observation_key"`
	BindingID      string `json:"binding_id"`
	ResourceID     string `json:"resource_id"`
	SubjectID      string `json:"subject_id"`
	Health         string `json:"health"`
	HealthRollup   string `json:"health_rollup"`
	Freshness      string `json:"freshness"`
	RecoveryCount  int    `json:"recovery_count"`
}

func valid(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0 && *v <= 100
}

func Evaluate(now time.Time, samples []Sample, interval int, p Policy) []Assessment {
	if interval < 3 || interval > 86400 {
		interval = 3
	}
	samples = append([]Sample(nil), samples...)
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].ObservedAt.Equal(samples[j].ObservedAt) {
			return samples[i].ID < samples[j].ID
		}
		return samples[i].ObservedAt.Before(samples[j].ObservedAt)
	})
	// One point per observation time; replayed/duplicate points cannot sustain a rule.
	clean := []Sample{}
	for _, s := range samples {
		if s.ObservedAt.After(now) || s.ObservedAt.After(s.ReceivedAt.Add(30*time.Second)) {
			continue
		}
		if len(clean) > 0 && clean[len(clean)-1].ObservedAt.Equal(s.ObservedAt) {
			clean[len(clean)-1] = s
		} else {
			clean = append(clean, s)
		}
	}
	// Do not bridge reboot or an observation gap into a sustained signal/forecast.
	if len(clean) > 0 {
		start := len(clean) - 1
		gap := time.Duration(monitoring.StaleAfterSeconds(interval)) * time.Second
		for start > 0 && clean[start-1].BootID == clean[len(clean)-1].BootID && clean[start].ObservedAt.Sub(clean[start-1].ObservedAt) <= gap {
			start--
		}
		clean = clean[start:]
	}
	assessments := []Assessment{threshold(now, clean, interval, "cpu.sustained", p.CPUWarning, p.CPUCritical, p.CPURecovery, false), threshold(now, clean, interval, "ram.sustained", p.RAMWarning, p.RAMCritical, p.RAMRecovery, true), forecast(now, clean, interval, p)}
	for i := range assessments {
		assessments[i].RuleVersion = "1"
		assessments[i].Policy = p
	}
	return assessments
}
func base(now time.Time, s []Sample, interval int, rule string) Assessment {
	a := Assessment{RuleID: rule, Status: "unknown", Severity: "none", Reason: "no_data", Evidence: []Point{}}
	if len(s) == 0 {
		return a
	}
	at := s[len(s)-1].ObservedAt
	a.ObservedAt = &at
	if now.Sub(at) > time.Duration(monitoring.StaleAfterSeconds(interval))*time.Second {
		a.Status = "stale"
		a.Reason = "telemetry_stale"
	} else {
		a.Reason = "insufficient_data"
	}
	return a
}
func threshold(now time.Time, s []Sample, interval int, rule string, warn, critical, recover float64, ram bool) Assessment {
	a := base(now, s, interval, rule)
	if len(s) == 0 || a.Status == "stale" {
		return a
	}
	points := []Point{}
	for i := len(s) - 1; i >= 0 && len(points) < 120; i-- {
		value := s[i].CPU
		if ram {
			value = s[i].RAM
		}
		if !valid(value) {
			break
		}
		points = append([]Point{{s[i].ID, s[i].ObservedAt, *value}}, points...)
		if len(points) >= 3 && points[len(points)-1].ObservedAt.Sub(points[0].ObservedAt).Seconds() >= float64(max(6, 2*interval)) {
			break
		}
	}
	if len(points) == 0 {
		a.Reason = "metric_missing_or_invalid"
		return a
	}
	a.Evidence = points
	v := points[len(points)-1].Value
	a.Value = &v
	a.WindowSeconds = points[len(points)-1].ObservedAt.Sub(points[0].ObservedAt).Seconds()
	mature := len(points) >= 3 && a.WindowSeconds >= float64(max(6, 2*interval))
	high, crit, low := true, true, true
	for _, pt := range points {
		high = high && pt.Value >= warn
		crit = crit && pt.Value >= critical
		low = low && pt.Value <= recover
	}
	if mature && high {
		a.Status = "warning"
		a.Severity = "warning"
		a.Reason = "sustained_threshold"
		if crit {
			a.Status = "critical"
			a.Severity = "critical"
		}
		return a
	}
	a.Status = "ok"
	a.Reason = "below_threshold"
	a.RecoveryEligible = mature && low
	if v >= warn {
		a.Status = "pending"
		a.Reason = "spike_not_sustained"
	}
	return a
}
func forecast(now time.Time, s []Sample, interval int, p Policy) Assessment {
	a := base(now, s, interval, "cpu.trend")
	if len(s) == 0 || a.Status == "stale" {
		return a
	}
	if s[len(s)-1].BootID == "" {
		a.Reason = "boot_identity_missing"
		return a
	}
	points := []Point{}
	for i := len(s) - 1; i >= 0 && len(points) < 120; i-- {
		if !valid(s[i].CPU) {
			break
		}
		points = append([]Point{{s[i].ID, s[i].ObservedAt, *s[i].CPU}}, points...)
	}
	a.Evidence = points
	if len(points) < 6 {
		return a
	}
	span := points[len(points)-1].ObservedAt.Sub(points[0].ObservedAt).Seconds()
	a.WindowSeconds = span
	if span < float64(max(30, 5*interval)) {
		return a
	}
	n := float64(len(points))
	var sumX, sumY float64
	for _, pt := range points {
		sumX += pt.ObservedAt.Sub(points[0].ObservedAt).Seconds()
		sumY += pt.Value
	}
	meanX, meanY := sumX/n, sumY/n
	var xx, xy, yy float64
	for _, pt := range points {
		x := pt.ObservedAt.Sub(points[0].ObservedAt).Seconds() - meanX
		y := pt.Value - meanY
		xx += x * x
		xy += x * y
		yy += y * y
	}
	if xx == 0 {
		return a
	}
	slope := xy / xx
	fit := 1.0
	if yy > 0 {
		fit = (xy * xy) / (xx * yy)
	}
	fit = min(1, max(0, fit))
	a.Slope = &slope
	a.Fit = &fit
	value := points[len(points)-1].Value
	a.Value = &value
	if value >= p.CPUCritical {
		sustained := threshold(now, s, interval, "cpu.sustained", p.CPUWarning, p.CPUCritical, p.CPURecovery, false)
		if sustained.Status == "critical" {
			a.Status = "critical"
			a.Severity = "critical"
			a.Reason = "forecast_threshold_already_reached"
		} else {
			a.Reason = "critical_value_not_sustained"
		}
		return a
	}
	if value < p.CPURecovery {
		a.Status = "ok"
		a.Reason = "cpu_below_recovery_threshold"
		a.RecoveryEligible = threshold(now, s, interval, "cpu.sustained", p.CPUWarning, p.CPUCritical, p.CPURecovery, false).RecoveryEligible
		return a
	}
	if fit < 0.9 {
		a.Reason = "unstable_trend"
		return a
	}
	if slope <= 0 {
		a.Status = "ok"
		a.Reason = "no_positive_risk_trend"
		a.RecoveryEligible = true
		return a
	}
	seconds := (p.CPUCritical - value) / slope
	a.Status = "ok"
	a.Reason = "threshold_outside_forecast_horizon"
	a.RecoveryEligible = true
	if seconds > 0 && seconds <= float64(p.ForecastHorizonSeconds) {
		a.Status = "warning"
		a.Severity = "warning"
		a.Reason = "threshold_expected_if_trend_continues"
		a.SecondsToCritical = &seconds
		a.RecoveryEligible = false
	}
	return a
}
