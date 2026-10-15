package assetvoip

import (
	"strings"
	"testing"
	"time"
)

func validInput() Input {
	return Input{BindingID: "bind-1", AgentID: "collector-1", TargetID: "pbx", Address: "pbx.example:5061", Transport: "tls", Source: "sip.options", ServiceKind: "pbx", Name: "Main PBX", ExpectedAssetRevision: 1, Enabled: true, Confirmed: true, Reason: "checked rack label"}
}

func TestReviewedInputRequiresExplicitIdentityAndConfirmation(t *testing.T) {
	if !ValidInput(validInput()) {
		t.Fatal("valid reviewed association rejected")
	}
	for name, change := range map[string]func(*Input){
		"missing_binding":       func(in *Input) { in.BindingID = "" },
		"unknown_kind":          func(in *Input) { in.ServiceKind = "switch" },
		"no_confirmation":       func(in *Input) { in.Confirmed = false },
		"no_revision":           func(in *Input) { in.ExpectedAssetRevision = 0 },
		"negative_revision":     func(in *Input) { in.ExpectedRevision = -1 },
		"unsupported_transport": func(in *Input) { in.Transport = "http" },
		"controls":              func(in *Input) { in.Name = "PBX\nother" },
		"long_address":          func(in *Input) { in.Address = strings.Repeat("a", 1025) },
		"no_reason":             func(in *Input) { in.Reason = "" },
		"unscoped_simulation":   func(in *Input) { in.Source = "simulated" },
	} {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			change(&in)
			if ValidInput(in) {
				t.Fatal("invalid association accepted")
			}
		})
	}
	modern := validInput()
	modern.Scope = "simulation-session"
	if !ValidInput(modern) {
		t.Fatal("typed simulated provenance is verified from archive, not Source string")
	}
}

func TestHistoryBounds(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Hour)
	if !ValidHistory(HistoryFilter{From: &now, To: &later, Limit: 200}) || !ValidPage(100, 100000) {
		t.Fatal("valid bound rejected")
	}
	for _, in := range []HistoryFilter{{Limit: 0}, {Limit: 201}, {Limit: 10, BeforeID: -1}, {Limit: 10, From: &later, To: &now}} {
		if ValidHistory(in) {
			t.Fatal("unbounded or inverted history accepted")
		}
	}
	far := now.Add(31 * 24 * time.Hour)
	if ValidHistory(HistoryFilter{From: &now, To: &far, Limit: 1}) || ValidPage(101, 0) {
		t.Fatal("oversized history accepted")
	}
}
