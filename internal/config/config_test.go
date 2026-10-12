package config

import "testing"

func TestConfiguration(t *testing.T) {
	for _, key := range []string{"DATABASE_URL", "HTTP_ADDRESS", "GRPC_ADDRESS", "AGENT_TIMEOUT", "TLS_CERT_FILE", "TLS_KEY_FILE", "TLS_CLIENT_CA_FILE", "DEFAULT_TELEMETRY_INTERVAL", "DEFAULT_INVENTORY_INTERVAL", "DEFAULT_TOPOLOGY_INTERVAL", "DEFAULT_TELEPHONY_INTERVAL", "AUTH_MODE", "AUTH_SESSION_TTL", "AUTH_PUBLIC_ORIGIN", "AUTH_COOKIE_SECURE"} {
		t.Setenv(key, "")
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, value string }{
		{"AGENT_TIMEOUT", "forever"}, {"AGENT_TIMEOUT", "0s"}, {"HTTP_ADDRESS", "localhost"},
		{"GRPC_ADDRESS", "localhost:70000"}, {"TLS_CERT_FILE", "cert.pem"}, {"TLS_CLIENT_CA_FILE", "ca.pem"},
		{"DEFAULT_TELEMETRY_INTERVAL", "2s"}, {"DEFAULT_TELEMETRY_INTERVAL", "24h1s"}, {"DEFAULT_TOPOLOGY_INTERVAL", "3.5s"},
		{"DEFAULT_TELEPHONY_INTERVAL", "1s"}, {"DEFAULT_TELEPHONY_INTERVAL", "24h1s"},
		{"AUTH_MODE", "optional"}, {"AUTH_SESSION_TTL", "0s"}, {"AUTH_SESSION_TTL", "25h"}, {"AUTH_PUBLIC_ORIGIN", "https://user:secret@example.test"}, {"AUTH_PUBLIC_ORIGIN", "http://localhost:8080/path"}, {"AUTH_COOKIE_SECURE", "maybe"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	t.Setenv("DEFAULT_TELEMETRY_INTERVAL", "24h")
	t.Setenv("DEFAULT_TELEPHONY_INTERVAL", "24h")
	c, err := Load()
	if err != nil || c.MonitoringDefaults.TelemetryIntervalSeconds != 86400 || c.TelephonyIntervalSeconds != 86400 {
		t.Fatalf("24h interval rejected: %+v %v", c, err)
	}
}
