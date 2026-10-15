package assets

import "testing"

func TestAutomaticMatchRequiresCorroboration(t *testing.T) {
	a := Signature{Kind: "network_device", Vendor: "eltex", Model: "mes", Serials: []string{"serial"}, StrongSerials: []string{"serial"}}
	if !AutoCompatible(a, a) {
		t.Fatal("verified network namespace not matched")
	}
	b := a
	b.Model = ""
	if AutoCompatible(a, b) {
		t.Fatal("missing model corroboration accepted")
	}
	b = a
	b.Kind = "host"
	a.Kind = "host"
	if AutoCompatible(a, b) {
		t.Fatal("host serial alone automatically matched")
	}
	a.UUIDs = []string{"uuid"}
	b.UUIDs = a.UUIDs
	a.StrongUUIDs = a.UUIDs
	b.StrongUUIDs = a.UUIDs
	if !AutoCompatible(a, b) {
		t.Fatal("corroborated UUID not matched")
	}
	b.Serials = []string{"conflicting"}
	if AutoCompatible(a, b) {
		t.Fatal("UUID concealed contradictory serial")
	}
}
