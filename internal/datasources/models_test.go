package datasources

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestSourceValidation(t *testing.T) {
	base := Input{Type: "ZABBIX", Name: "PROD", Endpoint: "https://monitor.example/api_jsonrpc.php", Enabled: true, SyncIntervalSeconds: 3}
	for _, endpoint := range []string{"http://monitor.example", "https://user:password@monitor.example", "https://monitor.example?token=secret", "https://monitor.example?", "https://monitor.example/#fragment", "file:///etc/passwd"} {
		v := base
		v.Endpoint = endpoint
		if _, err := v.Normalize(); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	if _, err := base.Normalize(); err != nil {
		t.Fatal(err)
	}
	base.SyncIntervalSeconds = 86400
	if _, err := base.Normalize(); err != nil {
		t.Fatal("24-hour interval rejected", err)
	}
	base.SyncIntervalSeconds = 86401
	if _, err := base.Normalize(); err == nil {
		t.Fatal("unbounded interval accepted")
	}
	base = Input{Type: "COLLECTOR", Name: "Collector", ConnectionRef: "agent-1", Enabled: true, SyncIntervalSeconds: 3}
	if _, err := base.Normalize(); err != nil {
		t.Fatal(err)
	}
	secret := "secret"
	base.Credential = &secret
	if _, err := base.Normalize(); err == nil {
		t.Fatal("Collector credential accepted")
	}
}

func TestCredentialAuthenticatedEncryption(t *testing.T) {
	v, err := NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	a, err := v.Seal("source-1", "private token")
	if err != nil {
		t.Fatal(err)
	}
	b, err := v.Seal("source-1", "private token")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) || bytes.Contains(a, []byte("private token")) {
		t.Fatal("encryption lacks nonce or exposes secret")
	}
	plain, err := v.Open("source-1", a)
	if err != nil || string(plain) != "private token" {
		t.Fatal("round trip", err)
	}
	clear(plain)
	if _, err = v.Open("source-2", a); err == nil {
		t.Fatal("ciphertext transplant accepted")
	}
	a[len(a)-1] ^= 1
	if _, err = v.Open("source-1", a); err == nil {
		t.Fatal("ciphertext tampering accepted")
	}
	var absent *Vault
	if _, err = absent.Seal("source-1", "secret"); err != ErrKeyUnavailable {
		t.Fatal(err)
	}
	if _, err = NewVault("invalid secret key"); err == nil {
		t.Fatal("invalid key accepted")
	}
}
