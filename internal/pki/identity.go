// Package pki implements the existing Agent certificate identity contract.
package pki

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func AgentURI(id string) (*url.URL, error) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || strings.ToLower(id) != id {
		return nil, errors.New("invalid canonical agent UUID")
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(raw) != 16 {
		return nil, errors.New("invalid agent UUID")
	}
	nonzero := false
	for _, b := range raw {
		nonzero = nonzero || b != 0
	}
	if !nonzero {
		return nil, errors.New("nil agent UUID")
	}
	return url.Parse("spiffe://infrapulse/agent/" + id)
}

func VerifyIdentity(cert *x509.Certificate, id string) error {
	uri, err := AgentURI(id)
	if err != nil || cert == nil || len(cert.URIs) != 1 || cert.URIs[0].String() != uri.String() {
		return errors.New("certificate identity does not match agent")
	}
	return nil
}

// AuthorizePeer accepts only a verified TLS client certificate, never forwarded
// headers or an unverified PeerCertificates entry. Recheck expiry on long streams.
func AuthorizePeer(ctx context.Context, id string) error {
	leaf, err := peerCertificate(ctx)
	if err != nil {
		return err
	}
	return VerifyIdentity(leaf, id)
}

func peerCertificate(ctx context.Context) (*x509.Certificate, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, errors.New("TLS peer required")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 || len(info.State.PeerCertificates) == 0 {
		return nil, errors.New("verified TLS client certificate required")
	}
	leaf := info.State.VerifiedChains[0][0]
	if !leaf.Equal(info.State.PeerCertificates[0]) {
		return nil, errors.New("TLS leaf mismatch")
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("client certificate expired or not yet valid")
	}
	return leaf, nil
}
