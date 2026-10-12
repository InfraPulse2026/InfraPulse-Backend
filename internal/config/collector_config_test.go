package config

import "testing"

func TestManagedCollectorConfigRequiresMTLSAndRESTAuth(t *testing.T) {
	for _, key := range []string{"TLS_CERT_FILE", "TLS_KEY_FILE", "TLS_CLIENT_CA_FILE", "PKI_ENABLED", "AUTH_MODE", "AUTH_PUBLIC_ORIGIN", "AUTH_COOKIE_SECURE"} {
		t.Setenv(key, "")
	}
	t.Setenv("COLLECTOR_CONFIG_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("managed plaintext accepted")
	}
	t.Setenv("TLS_CERT_FILE", "fixture-server.pem")
	t.Setenv("TLS_KEY_FILE", "fixture-server.key")
	t.Setenv("TLS_CLIENT_CA_FILE", "fixture-ca.pem")
	c, err := Load()
	if err != nil || !c.CollectorConfigEnabled {
		t.Fatal("managed mTLS configuration rejected", err)
	}
	t.Setenv("AUTH_MODE", "disabled")
	if _, err := Load(); err == nil {
		t.Fatal("anonymous management accepted")
	}
	t.Setenv("AUTH_MODE", "required")
	t.Setenv("TLS_CLIENT_CA_FILE", "")
	if _, err := Load(); err == nil {
		t.Fatal("server-only TLS management accepted")
	}
	t.Setenv("COLLECTOR_CONFIG_ENABLED", "maybe")
	if _, err := Load(); err == nil {
		t.Fatal("malformed flag")
	}
}
