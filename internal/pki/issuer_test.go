package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

const testID = "bf6e7560-9b04-4d28-ae98-5dfe7d4f2001"

func issuerFixture(t *testing.T) (*Issuer, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	issuer, err := NewIssuer(certPEM, keyPEM, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return issuer, certPEM, keyPEM
}

func csrFixture(t *testing.T, template *x509.CertificateRequest) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestIssuedCertificateMatchesAgentContractAndCannotBeCA(t *testing.T) {
	issuer, _, _ := issuerFixture(t)
	uri, _ := AgentURI(testID)
	// A malicious requested CA extension and subject must never be copied.
	constraints, _ := asn1.Marshal(struct{ CA bool }{true})
	csr := csrFixture(t, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "administrator"}, URIs: []*url.URL{uri}, ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: constraints}}})
	now := time.Now()
	result, err := issuer.Sign(testID, csr, now)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(result.Certificate)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(issuer.ca)
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if err = VerifyIdentity(leaf, testID); err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA || leaf.KeyUsage != x509.KeyUsageDigitalSignature || leaf.Subject.CommonName != "" {
		t.Fatal("CSR privileges copied")
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("client certificate usable as server")
	}
	encoded, _ := json.Marshal(result)
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil || len(fields) != 2 || fields["certificate"] == nil || fields["ca"] == nil {
		t.Fatal("Agent JSON response mismatch")
	}
	second, err := issuer.Sign(testID, csr, now)
	if err != nil || result.Serial == second.Serial || result.Fingerprint == second.Fingerprint {
		t.Fatal("certificate identity reused")
	}
}

func TestRejectCSRAndIssuerMisconfiguration(t *testing.T) {
	issuer, ca, key := issuerFixture(t)
	uri, _ := AgentURI(testID)
	good := csrFixture(t, &x509.CertificateRequest{URIs: []*url.URL{uri}})
	other, _ := AgentURI("bf6e7560-9b04-4d28-ae98-5dfe7d4f2002")
	cases := map[string][]byte{
		"missing":  csrFixture(t, &x509.CertificateRequest{}),
		"other":    csrFixture(t, &x509.CertificateRequest{URIs: []*url.URL{other}}),
		"extra":    csrFixture(t, &x509.CertificateRequest{URIs: []*url.URL{uri}, DNSNames: []string{"localhost"}}),
		"multiple": append(append([]byte{}, good...), good...),
		"junk":     []byte("garbage"), "large": make([]byte, 32769),
	}
	block, _ := pem.Decode(good)
	block.Bytes[len(block.Bytes)-1] ^= 1
	cases["signature"] = pem.EncodeToMemory(block)
	for name, csr := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := issuer.Sign(testID, csr, time.Now()); err == nil {
				t.Fatal("invalid CSR accepted")
			}
		})
	}
	for _, id := range []string{"agent-1", "00000000-0000-0000-0000-000000000000", "BF6E7560-9B04-4D28-AE98-5DFE7D4F2001"} {
		if _, err := issuer.Sign(id, good, time.Now()); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
	for _, ttl := range []time.Duration{0, 30 * time.Second, 91 * 24 * time.Hour} {
		if _, err := NewIssuer(ca, key, ttl); err == nil {
			t.Fatal("invalid lifetime accepted")
		}
	}
	if _, err := NewIssuer(append(append([]byte{}, ca...), ca...), key, time.Hour); err == nil {
		t.Fatal("multiple CA certificates accepted")
	}
	if _, err := issuer.Sign(testID, good, issuer.ca.NotAfter.Add(-30*time.Second)); err == nil {
		t.Fatal("near-expired CA accepted")
	}
	result, err := issuer.Sign(testID, good, issuer.ca.NotAfter.Add(-90*time.Minute))
	if err != nil || !result.NotAfter.Equal(issuer.ca.NotAfter) {
		t.Fatal("expiry must be capped to CA")
	}
}

func TestPeerRequiresVerifiedCertificateAndMatchingUUID(t *testing.T) {
	issuer, _, _ := issuerFixture(t)
	uri, _ := AgentURI(testID)
	result, err := issuer.Sign(testID, csrFixture(t, &x509.CertificateRequest{URIs: []*url.URL{uri}}), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(result.Certificate)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	ctxFor := func(state tls.ConnectionState) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: state}})
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf, issuer.ca}}}
	if err := AuthorizePeer(ctxFor(state), testID); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizePeer(ctxFor(state), "bf6e7560-9b04-4d28-ae98-5dfe7d4f2002"); err == nil {
		t.Fatal("identity theft accepted")
	}
	if err := AuthorizePeer(context.Background(), testID); err == nil {
		t.Fatal("missing peer accepted")
	}
	state.VerifiedChains = nil
	if err := AuthorizePeer(ctxFor(state), testID); err == nil {
		t.Fatal("unverified certificate accepted")
	}
	state.VerifiedChains = [][]*x509.Certificate{{leaf, issuer.ca}}
	leaf.NotAfter = time.Now().Add(-time.Second)
	if err := AuthorizePeer(ctxFor(state), testID); err == nil {
		t.Fatal("expired stream certificate accepted")
	}
}

func TestIssuerRejectsCertificateOutsideCANameConstraints(t *testing.T) {
	issuer, _, keyPEM := issuerFixture(t)
	caTemplate := *issuer.ca
	caTemplate.PermittedDNSDomainsCritical = true
	caTemplate.PermittedURIDomains = []string{"another-domain.test"}
	der, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, issuer.key.Public(), issuer.key)
	if err != nil {
		t.Fatal(err)
	}
	constrained, err := NewIssuer(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := AgentURI(testID)
	if _, err = constrained.Sign(testID, csrFixture(t, &x509.CertificateRequest{URIs: []*url.URL{uri}}), time.Now()); err == nil {
		t.Fatal("unusable certificate issued despite CA restrictions")
	}
}
