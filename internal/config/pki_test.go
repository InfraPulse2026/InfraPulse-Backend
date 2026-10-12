package config

import "testing"

func TestPKIRequiresAuthTLSAndBoundedLifetime(t *testing.T) {
	for _, key := range []string{"PKI_ENABLED", "PKI_ADDRESS", "PKI_CA_FILE", "PKI_CA_KEY_FILE", "PKI_TLS_CERT_FILE", "PKI_TLS_KEY_FILE", "PKI_CERT_LIFETIME", "AUTH_MODE", "HTTP_ADDRESS", "TLS_CERT_FILE", "TLS_KEY_FILE", "TLS_CLIENT_CA_FILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("PKI_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("unconfigured PKI enabled")
	}
	for key, value := range map[string]string{"TLS_CERT_FILE": "server.pem", "TLS_KEY_FILE": "server.key", "TLS_CLIENT_CA_FILE": "ca.pem", "PKI_CA_FILE": "ca.pem", "PKI_CA_KEY_FILE": "ca.key"} {
		t.Setenv(key, value)
	}
	if cfg, err := Load(); err != nil || !cfg.PKI.Enabled || cfg.PKI.CertFile != "server.pem" {
		t.Fatalf("valid PKI config rejected: %v", err)
	}
	for _, tc := range []struct{ key, value string }{{"PKI_CERT_LIFETIME", "30s"}, {"PKI_CERT_LIFETIME", "2161h"}, {"PKI_ADDRESS", "localhost:70000"}, {"AUTH_MODE", "disabled"}, {"PKI_ENABLED", "maybe"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid PKI configuration accepted")
			}
		})
	}
}
