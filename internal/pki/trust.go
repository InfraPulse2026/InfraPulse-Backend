package pki

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
)

// CheckTrust prevents issuing identities that the configured gRPC CA rejects.
// For this deployment the issuing root must be directly present in client CA.
func CheckTrust(issuerPEM, trustPEM []byte) error {
	block, _ := pem.Decode(issuerPEM)
	if block == nil {
		return errors.New("missing issuing CA")
	}
	issuer, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	for {
		block, trustPEM = pem.Decode(trustPEM)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err == nil && cert.Equal(issuer) {
			return nil
		}
	}
	return errors.New("issuing CA must be present in TLS_CLIENT_CA_FILE")
}
