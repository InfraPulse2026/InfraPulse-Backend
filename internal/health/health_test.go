package health

import (
	"math"
	"testing"
	"time"
)

func series(now time.Time, values []float64, step time.Duration) []Sample {
	items := []Sample{}
	for i, value := range values {
		v := value
		items = append(items, Sample{ID: int64(i + 1), ObservedAt: now.Add(-time.Duration(len(values)-1-i) * step), ReceivedAt: now, BootID: "boot-1", CPU: &v, RAM: &v})
	}
	return items
}
func TestSustainedSignalRecoveryAndSpike(t *testing.T) {
	now := time.Now()
	p := Defaults()
	cases := []struct {
		values  []float64
		status  string
		recover bool
	}{{[]float64{81, 82, 83}, "warning", false}, {[]float64{95, 96, 97}, "critical", false}, {[]float64{40, 40, 98}, "pending", false}, {[]float64{60, 60, 60}, "ok", true}, {[]float64{90, 60, 60}, "ok", false}, {[]float64{75, 75, 75}, "ok", false}}
	for _, tc := range cases {
		a := Evaluate(now, series(now, tc.values, 3*time.Second), 3, p)[0]
		if a.Status != tc.status || a.RecoveryEligible != tc.recover {
			t.Fatalf("values=%v assessment=%+v", tc.values, a)
		}
	}
	spike := Evaluate(now, series(now, []float64{50, 50, 50, 50, 50, 99}, 6*time.Second), 3, p)
	for _, a := range spike {
		if a.Status == "warning" || a.Status == "critical" {
			t.Fatalf("single spike produced incident: %+v", a)
		}
	}
}
func TestForecastBeforeWarningAndNoInventedProbability(t *testing.T) {
	now := time.Now()
	a := Evaluate(now, series(now, []float64{50, 55, 60, 65, 70, 75}, 6*time.Second), 3, Defaults())[2]
	if a.Status != "warning" || a.Value == nil || *a.Value >= 80 || a.SecondsToCritical == nil || math.Abs(*a.SecondsToCritical-24) > 0.001 || a.Fit == nil || *a.Fit < 0.99 {
		t.Fatalf("early trend not detected: %+v", a)
	}
	if len(a.Evidence) != 6 || a.WindowSeconds != 30 {
		t.Fatal("missing reproducible evidence")
	}
	for _, values := range [][]float64{{75, 72, 70, 68, 65, 60}, {70, 80, 60, 83, 62, 79}} {
		v := Evaluate(now, series(now, values, 6*time.Second), 3, Defaults())[2]
		if v.Status == "warning" || v.Status == "critical" {
			t.Fatalf("bad trend forecast: %+v", v)
		}
	}
}
func TestMissingStaleDuplicateBootAndGapDoNotRecover(t *testing.T) {
	now := time.Now()
	items := series(now, []float64{90, 91, 92}, 3*time.Second)
	stale := Evaluate(now.Add(12*time.Second), items, 3, Defaults())[0]
	if stale.Status != "stale" || stale.RecoveryEligible {
		t.Fatal("stale data healthy")
	}
	duplicate := []Sample{items[0], items[0], items[0]}
	if v := Evaluate(now, duplicate, 3, Defaults())[0]; v.Status == "warning" {
		t.Fatal("duplicate observation sustained threshold")
	}
	reboot := series(now, []float64{90, 91, 92}, 3*time.Second)
	reboot[2].BootID = "boot-2"
	if v := Evaluate(now, reboot, 3, Defaults())[0]; v.Status != "pending" {
		t.Fatal("reboot continuity not reset")
	}
	gap := series(now, []float64{90, 91, 92}, time.Minute)
	if v := Evaluate(now, gap, 3, Defaults())[0]; v.Status != "pending" {
		t.Fatal("observation gap bridged")
	}
	items[2].CPU = nil
	if v := Evaluate(now, items, 3, Defaults())[0]; v.Status != "unknown" || v.RecoveryEligible {
		t.Fatal("missing CPU treated as healthy")
	}
	future := series(now.Add(time.Minute), []float64{90, 91, 92}, 3*time.Second)
	if v := Evaluate(now, future, 3, Defaults())[0]; v.Status != "unknown" {
		t.Fatal("future metric triggered rule")
	}
}

func TestFasterCadenceStillDetectsSustainedCPU(t *testing.T) {
	values := make([]float64, 10)
	for i := range values {
		values[i] = 85
	}
	now := time.Now()
	a := Evaluate(now, series(now, values, time.Second), 3, Defaults())[0]
	if a.Status != "warning" || len(a.Evidence) < 7 || a.WindowSeconds < 6 {
		t.Fatalf("fast collector never sustained: %+v", a)
	}
}

func TestForecastSingleDipDoesNotConfirmRecovery(t *testing.T) {
	now := time.Now()
	a := Evaluate(now, series(now, []float64{70, 75, 80, 85, 90, 60}, 6*time.Second), 3, Defaults())[2]
	if a.RecoveryEligible {
		t.Fatal("single low point recovered trend incident")
	}
}

func TestRAMZeroMissingAndPolicyEvidence(t *testing.T) {
	now := time.Now()
	items := series(now, []float64{81, 82, 83}, 3*time.Second)
	zero := 0.0
	for i := range items {
		items[i].RAM = &zero
	}
	result := Evaluate(now, items, 3, Defaults())
	if result[1].Status != "ok" || !result[1].RecoveryEligible || result[1].Value == nil || *result[1].Value != 0 {
		t.Fatal("known zero RAM lost")
	}
	if result[0].RuleVersion != "1" || result[0].Policy.CPUWarning != 80 {
		t.Fatal("applied rule policy missing from evidence")
	}
	items[2].RAM = nil
	if v := Evaluate(now, items, 3, Defaults())[1]; v.Status != "unknown" || v.RecoveryEligible {
		t.Fatal("missing RAM recovered")
	}
}
