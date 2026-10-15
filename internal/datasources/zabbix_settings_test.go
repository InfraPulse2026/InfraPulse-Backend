package datasources

import (
	"reflect"
	"testing"
)

func TestZabbixSettingsBoundsAndIdentitySelection(t *testing.T) {
	z, err := (ZabbixSettings{HostIDs: []string{"10", "2", "1"}}).Normalize()
	if err != nil || z.PageSize != 20 || z.DeviceType != "unknown" || z.VerifiedInventory || !reflect.DeepEqual(z.HostIDs, []string{"1", "2", "10"}) {
		t.Fatal(z, err)
	}
	for _, ids := range [][]string{{"0"}, {"01"}, {"-1"}, {"1", "1"}, {"18446744073709551616"}, {"x"}} {
		if _, err = (ZabbixSettings{HostIDs: ids}).Normalize(); err == nil {
			t.Fatal("unsafe host selection accepted", ids)
		}
	}
	for _, z := range []ZabbixSettings{{PageSize: 51}, {PageSize: -1}, {DeviceType: "invented"}} {
		if _, err = z.Normalize(); err == nil {
			t.Fatal("unbounded or inferred settings")
		}
	}
	in := Input{Type: "PROMETHEUS", Name: "wrong adapter", Endpoint: "https://monitor.example", SyncIntervalSeconds: 3, Configuration: Settings{Zabbix: &ZabbixSettings{}}}
	if _, err = in.Normalize(); err == nil {
		t.Fatal("Zabbix settings crossed source type")
	}
}
