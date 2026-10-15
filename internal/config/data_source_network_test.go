package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearNetworkEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"DATA_SOURCE_ALLOWED_ORIGINS", "DATA_SOURCE_ALLOWED_CIDRS", "DATA_SOURCE_ALLOW_LOOPBACK", "DATA_SOURCE_CA_FILES"} {
		t.Setenv(name, "")
	}
}
func TestSourceNetworkDeploymentConfiguration(t *testing.T) {
	clearNetworkEnvironment(t)
	p, err := loadSourceNetwork(true)
	if err != nil || p.CheckEndpoint("https://monitor.example", "") == nil {
		t.Fatal("default permits network", err)
	}
	t.Setenv("DATA_SOURCE_ALLOWED_ORIGINS", "https://monitor.example,https://monitor.example:8443")
	t.Setenv("DATA_SOURCE_ALLOWED_CIDRS", "10.20.0.0/16,fd12:3456::/32")
	p, err = loadSourceNetwork(true)
	if err != nil || p.CheckEndpoint("https://monitor.example:8443/api", "") != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_SOURCE_ALLOW_LOOPBACK", "true")
	if _, err = loadSourceNetwork(false); err == nil {
		t.Fatal("loopback enabled with a public listener")
	}
	t.Setenv("DATA_SOURCE_ALLOW_LOOPBACK", "false")
	t.Setenv("DATA_SOURCE_ALLOWED_CIDRS", "private-secret-invalid")
	if _, err = loadSourceNetwork(true); err == nil || strings.Contains(err.Error(), "private-secret-invalid") {
		t.Fatal("bad CIDR accepted or echoed", err)
	}
	t.Setenv("DATA_SOURCE_ALLOWED_CIDRS", "10.20.0.0/16,")
	if _, err = loadSourceNetwork(true); err == nil {
		t.Fatal("empty allowlist entry accepted")
	}
}

func TestSourceNetworkCAFilesAndPrivateKeyRejection(t *testing.T) {
	clearNetworkEnvironment(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = os.WriteFile(path, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	files, _ := json.Marshal(map[string]string{"lab": path})
	t.Setenv("DATA_SOURCE_CA_FILES", string(files))
	p, err := loadSourceNetwork(true)
	if err != nil || len(p.CARefs()) != 1 || p.CARefs()[0] != "lab" {
		t.Fatal("CA reference unavailable", err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})...)
	if err = os.WriteFile(path, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadSourceNetwork(true); err == nil {
		t.Fatal("private key file accepted as public trust bundle")
	}
	t.Setenv("DATA_SOURCE_CA_FILES", `null`)
	if _, err = loadSourceNetwork(true); err == nil {
		t.Fatal("invalid CA mapping accepted")
	}
}
