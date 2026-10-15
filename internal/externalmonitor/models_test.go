package externalmonitor

import "testing"

func TestNumericUnitsPrecisionAndUnavailableInputs(t *testing.T) {
	for _, tc := range []struct{ raw, multiplier, unit, want string }{{"18446744073709551615", "1", "bytes", "18446744073709551615"}, {"125", "0.001", "seconds", "0.125"}, {"800", "0.125", "bytes_per_second", "100"}, {"1e2", "1", "percent", "100"}} {
		value, ok := NormalizeValue(tc.raw, Mapping{Multiplier: tc.multiplier, Unit: tc.unit})
		if !ok || value != tc.want {
			t.Fatal(tc, value, ok)
		}
	}
	for _, raw := range []string{"NaN", "Inf", "token=secret", "1/2", "-2", "1e999"} {
		if _, ok := NormalizeValue(raw, Mapping{Multiplier: "1", Unit: "bytes"}); ok {
			t.Fatal("non-metric or unsafe value accepted", raw)
		}
	}
	for _, raw := range []string{"101", "100.0001"} {
		if _, ok := NormalizeValue(raw, Mapping{Multiplier: "1", Unit: "percent"}); ok {
			t.Fatal("invalid percent accepted")
		}
	}
	if _, ok := NormalizeValue("2", Mapping{Multiplier: "1", Unit: "boolean"}); ok {
		t.Fatal("non-boolean availability")
	}
	m := Mapping{HostID: "1", ItemID: "901", Metric: "cpu.utilization", SourceUnit: "%", Unit: "percent"}
	normal, err := m.Normalize()
	if err != nil || normal.Multiplier != "1" || normal.MaxAgeSeconds != 300 {
		t.Fatal(normal, err)
	}
	m.Unit = "bytes"
	if _, err = m.Normalize(); err == nil {
		t.Fatal("guessed unit accepted")
	}
}
