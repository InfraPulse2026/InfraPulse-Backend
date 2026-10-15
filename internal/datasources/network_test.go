package datasources

import (
	"net/netip"
	"testing"
)

func TestNetworkPolicyRequiresBothOriginAndCIDR(t *testing.T) {
	p, err := NewNetworkPolicy(NetworkConfig{AllowedOrigins: []string{"https://monitor.example"}, AllowedCIDRs: []string{"10.20.0.0/16"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.CheckEndpoint("https://MONITOR.example:443/api_jsonrpc.php", ""); err != nil {
		t.Fatal("canonical origin", err)
	}
	for _, endpoint := range []string{"https://other.example", "https://monitor.example:444", "http://monitor.example", "https://monitor.example?token=secret"} {
		if p.CheckEndpoint(endpoint, "") == nil {
			t.Fatal("escaped origin", endpoint)
		}
	}
	if p.CheckEndpoint("https://monitor.example", "unknown-ca") == nil {
		t.Fatal("unknown CA accepted")
	}
	if p.CheckAddress(netip.MustParseAddr("10.20.1.2")) != nil {
		t.Fatal("allowed subnet denied")
	}
	if p.CheckAddress(netip.MustParseAddr("10.21.1.2")) == nil {
		t.Fatal("outside subnet accepted")
	}
	deny, _ := NewNetworkPolicy(NetworkConfig{})
	if code, retry := Outcome(ErrNetworkPolicy); code != "network_policy_rejected" || retry {
		t.Fatal("constructor policy error misclassified", code)
	}
	if deny.CheckEndpoint("https://monitor.example", "") == nil || deny.CheckAddress(netip.MustParseAddr("10.20.1.2")) == nil {
		t.Fatal("empty deployment policy failed open")
	}
}

func TestNetworkPolicyHardDeniesMetadataAndSpecialAddresses(t *testing.T) {
	p, err := NewNetworkPolicy(NetworkConfig{AllowedCIDRs: []string{"0.0.0.0/0", "::/0"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"127.0.0.1", "::1", "169.254.169.254", "::ffff:169.254.169.254", "100.100.100.200", "fd00:ec2::254", "0.0.0.0", "0.1.2.3", "255.255.255.255", "224.0.0.1", "fe80::1", "ff02::1", "::", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1", "2001::1"} {
		if err = p.CheckAddress(netip.MustParseAddr(raw)); err == nil {
			t.Fatal("special target permitted", raw)
		}
	}
	loop, _ := NewNetworkPolicy(NetworkConfig{AllowedCIDRs: []string{"127.0.0.0/8", "::1/128"}, AllowLoopback: true})
	if loop.CheckAddress(netip.MustParseAddr("127.0.0.1")) != nil || loop.CheckAddress(netip.MustParseAddr("::ffff:127.0.0.1")) != nil {
		t.Fatal("explicit lab loopback denied")
	}
}

func TestEndpointCanonicalizationAndCAReference(t *testing.T) {
	if !SameEndpoint("https://MONITOR.example/api", "https://monitor.example:443/api") || SameEndpoint("https://monitor.example/api", "https://monitor.example:444/api") {
		t.Fatal("endpoint comparison")
	}
	for _, endpoint := range []string{"https://host:0", "https://host:65536", "https://host:", "https://-host.example", "https://host..example", "https://host/../secret", "https://host/%2e%2e/secret", "https://host/#", "https://host/path\\other"} {
		if _, _, err := parseEndpoint(endpoint); err == nil {
			t.Fatal("invalid endpoint accepted", endpoint)
		}
	}
	in := Input{Type: "ZABBIX", Name: "fixture", Endpoint: "https://monitor.example", SyncIntervalSeconds: 3, Configuration: Settings{TLSCARef: "C:/secret/ca.pem"}}
	if _, err := in.Normalize(); err == nil {
		t.Fatal("filesystem path accepted as CA reference")
	}
	in.Configuration.TLSCARef = "lab-ca"
	if _, err := in.Normalize(); err != nil {
		t.Fatal("named CA rejected", err)
	}
}
