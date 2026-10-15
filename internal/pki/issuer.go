package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"time"
)

// Issuer never receives or creates an Agent private key. Sign produces a
// certificate only; enrollment authorization and durable registration belong
// to the service layer, which must commit before returning it to an Agent.
type Issuer struct {
	ca       *x509.Certificate
	key      crypto.Signer
	lifetime time.Duration
}
type Issued struct {
	Certificate []byte    `json:"certificate"`
	CA          []byte    `json:"ca"`
	Fingerprint string    `json:"-"`
	Serial      string    `json:"-"`
	NotBefore   time.Time `json:"-"`
	NotAfter    time.Time `json:"-"`
}

func NewIssuer(caPEM, keyPEM []byte, lifetime time.Duration) (*Issuer, error) {
	if lifetime < time.Minute || lifetime > 90*24*time.Hour {
		return nil, errors.New("certificate lifetime must be 1m..2160h")
	}
	pair, err := tls.X509KeyPair(caPEM, keyPEM)
	if err != nil || len(pair.Certificate) != 1 {
		return nil, errors.New("one matching issuing CA certificate and key required")
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || !ca.BasicConstraintsValid || !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("invalid issuing CA")
	}
	if len(ca.ExtKeyUsage) > 0 {
		clientAllowed := false
		for _, usage := range ca.ExtKeyUsage {
			clientAllowed = clientAllowed || usage == x509.ExtKeyUsageAny || usage == x509.ExtKeyUsageClientAuth
		}
		if !clientAllowed {
			return nil, errors.New("issuing CA excludes client authentication")
		}
	}
	now := time.Now()
	if now.Before(ca.NotBefore) || !now.Add(time.Minute).Before(ca.NotAfter) {
		return nil, errors.New("issuing CA not valid")
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("issuing key is not a signer")
	}
	return &Issuer{ca: ca, key: key, lifetime: lifetime}, nil
}

func (i *Issuer) Sign(id string, csrPEM []byte, now time.Time) (Issued, error) {
	var result Issued
	uri, err := AgentURI(id)
	if err != nil {
		return result, err
	}
	if len(csrPEM) > 32<<10 {
		return result, errors.New("CSR too large")
	}
	if !bytes.HasPrefix(bytes.TrimSpace(csrPEM), []byte("-----BEGIN CERTIFICATE REQUEST-----")) {
		return result, errors.New("CSR PEM must not contain a preamble")
	}
	block, rest := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return result, errors.New("one CSR PEM required")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return result, errors.New("invalid CSR signature")
	}
	key, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return result, errors.New("Agent CSR requires ECDSA P-256")
	}
	if len(csr.URIs) != 1 || csr.URIs[0].String() != uri.String() || len(csr.DNSNames) != 0 || len(csr.IPAddresses) != 0 || len(csr.EmailAddresses) != 0 {
		return result, errors.New("CSR identity mismatch or additional SANs")
	}
	now = now.UTC().Truncate(time.Second)
	expires := now.Add(i.lifetime)
	if expires.After(i.ca.NotAfter) {
		expires = i.ca.NotAfter
	}
	if now.Before(i.ca.NotBefore) || expires.Sub(now) < time.Minute {
		return result, errors.New("issuing CA validity insufficient")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159))
	if err != nil {
		return result, err
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	before := now.Add(-time.Minute)
	if before.Before(i.ca.NotBefore) {
		before = i.ca.NotBefore
	}
	// Deliberately do not copy Subject, Extensions or CA privileges from the CSR.
	template := &x509.Certificate{SerialNumber: serial, NotBefore: before, NotAfter: expires, URIs: []*url.URL{uri}, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, i.ca, key, i.key)
	if err != nil {
		return result, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return result, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(i.ca)
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return result, errors.New("issued certificate does not satisfy CA constraints")
	}
	digest := sha256.Sum256(der)
	return Issued{Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.ca.Raw}), Fingerprint: hex.EncodeToString(digest[:]), Serial: serial.Text(16), NotBefore: before, NotAfter: expires}, nil
}
