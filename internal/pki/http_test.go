package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type rejectRepository struct{ commits int }

func (r *rejectRepository) PKICommitIssue(context.Context, string, []byte, string, Issued) error {
	r.commits++
	return ErrDenied
}
func (*rejectRepository) PKICreateToken(context.Context, Token) error { return ErrDenied }
func (*rejectRepository) PKITokens(context.Context, string, int) ([]Token, error) {
	return nil, ErrDenied
}
func (*rejectRepository) PKIRevokeToken(context.Context, string) error { return ErrDenied }
func (*rejectRepository) PKICertificateActive(context.Context, string, string) error {
	return ErrDenied
}
func (*rejectRepository) PKICertificates(context.Context, string, int) ([]Certificate, error) {
	return nil, ErrDenied
}
func (*rejectRepository) PKIRevokeCertificate(context.Context, string) (string, error) {
	return "", ErrDenied
}
func (*rejectRepository) PKIAudit(context.Context, string, int) ([]Audit, error) {
	return nil, ErrDenied
}

func TestEnrollmentRejectsUnsafeRequestsWithoutIssuance(t *testing.T) {
	issuer, _, _ := issuerFixture(t)
	repo := &rejectRepository{}
	service := &Service{Repository: repo, Issuer: issuer}
	uri, _ := AgentURI(testID)
	body, _ := json.Marshal(map[string]any{"agent_id": testID, "csr": csrFixture(t, &x509.CertificateRequest{URIs: []*url.URL{uri}})})
	for _, tc := range []struct {
		name, path, auth string
		tls              bool
		want             int
	}{
		{"plaintext", "/enroll", "Bearer ip_enroll_" + strings.Repeat("a", 64), false, 400},
		{"missing token", "/enroll", "", true, 401},
		{"query token", "/enroll?token=secret", "", true, 400},
		{"renew without client cert", "/renew", "", true, 401},
		{"renew bearer forbidden", "/renew", "Bearer ip_enroll_" + strings.Repeat("a", 64), true, 401},
		{"invalid bootstrap", "/enroll", "Bearer ip_enroll_" + strings.Repeat("a", 64), true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://localhost"+tc.path, strings.NewReader(string(body)))
			if !tc.tls {
				req.TLS = nil
			} else {
				req.TLS = &tls.ConnectionState{}
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			service.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
		})
	}
	if repo.commits != 1 {
		t.Fatal("unauthenticated requests reached issuance")
	}
}
func TestPKIInitDoesNotOverwriteAndTrustMatches(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "pki")
	if err := InitCA(directory, []string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(filepath.Join(directory, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(directory, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewIssuer(ca, key, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err = CheckTrust(ca, ca); err != nil {
		t.Fatal(err)
	}
	if err = CheckTrust(ca, []byte("wrong CA")); err == nil {
		t.Fatal("wrong gRPC trust accepted")
	}
	if err = InitCA(directory, []string{"other.test"}); err == nil {
		t.Fatal("existing credentials overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(directory, "ca.key"))
	if string(after) != string(key) {
		t.Fatal("issuing key changed")
	}
}
