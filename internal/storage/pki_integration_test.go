package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
)

const pkiID = "bf6e7560-9b04-4d28-ae98-5dfe7d4f2001"

func pkiIssued(label string) pki.Issued {
	hash := sha256.Sum256([]byte(label))
	now := time.Now().UTC().Truncate(time.Second)
	return pki.Issued{Fingerprint: hex.EncodeToString(hash[:]), Serial: label, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
}
func pkiTokenFixture(t *testing.T, s *Store) (context.Context, pki.Token) {
	t.Helper()
	_, actor := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), actor)
	token, err := pki.NewToken(pkiID, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PKICreateToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	return ctx, token
}
func TestPKIConcurrentEnrollmentOneWinnerAndSecretsNotStored(t *testing.T) {
	s := integrationStore(t)
	ctx, token := pkiTokenFixture(t, s)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- s.PKICommitIssue(ctx, pkiID, token.Hash, "", pkiIssued(string(rune('a'+i))))
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, pki.ErrDenied) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("enrollment winners=%d", success)
	}
	tokens, err := s.PKITokens(ctx, pkiID, 100)
	if err != nil || len(tokens) != 1 || tokens[0].UsedAt == nil || tokens[0].Secret != "" || len(tokens[0].Hash) != 0 {
		t.Fatal("token projection leaked secret or missing use")
	}
	var stored string
	if err = s.pool.QueryRow(ctx, `SELECT encode(token_hash,'hex') FROM pki_bootstrap_tokens`).Scan(&stored); err != nil || stored == token.Secret {
		t.Fatal("raw token persisted")
	}
	certs, err := s.PKICertificates(ctx, pkiID, 100)
	if err != nil || len(certs) != 1 {
		t.Fatal("wrong certificate count")
	}
	if err = s.PKICertificateActive(ctx, pkiID, certs[0].Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err = s.PKICertificateActive(ctx, "bf6e7560-9b04-4d28-ae98-5dfe7d4f2002", certs[0].Fingerprint); !errors.Is(err, pki.ErrDenied) {
		t.Fatal("cross-agent certificate accepted")
	}
}
func TestPKIRenewalRevocationAndCurrentAdminCheck(t *testing.T) {
	s := integrationStore(t)
	ctx, token := pkiTokenFixture(t, s)
	first := pkiIssued("first")
	second := pkiIssued("second")
	if err := s.PKICommitIssue(ctx, pkiID, token.Hash, "", first); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICommitIssue(ctx, pkiID, nil, first.Fingerprint, second); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICertificateActive(ctx, pkiID, first.Fingerprint); err != nil {
		t.Fatal("renewal prematurely revoked old credential")
	}
	if _, err := s.PKIRevokeCertificate(context.Background(), first.Fingerprint); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("anonymous revoke allowed")
	}
	if _, err := s.PKIRevokeCertificate(ctx, first.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICertificateActive(ctx, pkiID, first.Fingerprint); !errors.Is(err, pki.ErrDenied) {
		t.Fatal("revoked certificate accepted")
	}
	if err := s.PKICommitIssue(ctx, pkiID, nil, first.Fingerprint, pkiIssued("bad-renew")); !errors.Is(err, pki.ErrDenied) {
		t.Fatal("revoked renewal accepted")
	}
	if err := s.PKICertificateActive(ctx, pkiID, second.Fingerprint); err != nil {
		t.Fatal("new certificate lost")
	}
	// Registry/token/audit survive startup migrations; no token or cert replay.
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICommitIssue(ctx, pkiID, token.Hash, "", pkiIssued("restart-replay")); !errors.Is(err, pki.ErrDenied) {
		t.Fatal("restart reused bootstrap")
	}
	audit, err := s.PKIAudit(ctx, pkiID, 100)
	if err != nil || len(audit) != 4 {
		t.Fatal("missing PKI audit")
	}
	p, _ := access.Current(ctx)
	if err = s.AccessLogout(ctx, p, true, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := pki.NewToken(pkiID, time.Hour)
	if err = s.PKICreateToken(ctx, fresh); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("logged-out administrator minted token")
	}
}
func TestPKITokenRollbackExpiredRevokedAndWrongIdentity(t *testing.T) {
	s := integrationStore(t)
	ctx, token := pkiTokenFixture(t, s)
	// Simulate failure AFTER consuming the token but BEFORE transaction commit.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE pki_certificates ADD CONSTRAINT test_reject CHECK(serial<>'reject')`); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICommitIssue(ctx, pkiID, token.Hash, "", pkiIssued("reject")); err == nil {
		t.Fatal("fixture SQL failure missing")
	}
	tokens, _ := s.PKITokens(ctx, pkiID, 100)
	if tokens[0].UsedAt != nil {
		t.Fatal("failed SQL burned token")
	}
	if err := s.PKICommitIssue(ctx, "bf6e7560-9b04-4d28-ae98-5dfe7d4f2002", token.Hash, "", pkiIssued("wrong-id")); !errors.Is(err, pki.ErrDenied) {
		t.Fatal("wrong UUID accepted")
	}
	if err := s.PKICommitIssue(ctx, pkiID, token.Hash, "", pkiIssued("valid")); err != nil {
		t.Fatal(err)
	}
	second, _ := pki.NewToken(pkiID, time.Hour)
	if err := s.PKICreateToken(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := s.PKIRevokeToken(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICommitIssue(ctx, pkiID, second.Hash, "", pkiIssued("revoked-token")); !errors.Is(err, pki.ErrDenied) {
		t.Fatal("revoked token accepted")
	}
	third, _ := pki.NewToken(pkiID, time.Hour)
	if err := s.PKICreateToken(ctx, third); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE pki_bootstrap_tokens SET expires_at=now()-interval '1 second' WHERE id=$1`, third.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICommitIssue(ctx, pkiID, third.Hash, "", pkiIssued("expired-token")); !errors.Is(err, pki.ErrDenied) {
		t.Fatal("expired token accepted")
	}
}

func TestPKILimitsAndAuditFailureAreAtomic(t *testing.T) {
	s := integrationStore(t)
	ctx, token := pkiTokenFixture(t, s)
	if _, err := s.pool.Exec(ctx, `ALTER TABLE pki_audit ADD CONSTRAINT test_reject_audit CHECK(action<>'enrolled')`); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICommitIssue(ctx, pkiID, token.Hash, "", pkiIssued("audit-failure")); err == nil {
		t.Fatal("audit SQL failure missing")
	}
	tokens, _ := s.PKITokens(ctx, pkiID, 100)
	certs, _ := s.PKICertificates(ctx, pkiID, 100)
	if tokens[0].UsedAt != nil || len(certs) != 0 {
		t.Fatal("audit failure left partial issuance")
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE pki_audit DROP CONSTRAINT test_reject_audit`); err != nil {
		t.Fatal(err)
	}
	first := pkiIssued("limit-first")
	if err := s.PKICommitIssue(ctx, pkiID, token.Hash, "", first); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		if err := s.PKICommitIssue(ctx, pkiID, nil, first.Fingerprint, pkiIssued(string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	fresh, _ := pki.NewToken(pkiID, time.Hour)
	if err := s.PKICreateToken(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICommitIssue(ctx, pkiID, fresh.Hash, "", pkiIssued("over-limit")); !errors.Is(err, pki.ErrConflict) {
		t.Fatal("certificate limit not enforced")
	}
	tokens, _ = s.PKITokens(ctx, pkiID, 100)
	for _, v := range tokens {
		if v.ID == fresh.ID && v.UsedAt != nil {
			t.Fatal("capacity limit burned bootstrap token")
		}
	}
	if _, err := s.PKIRevokeCertificate(ctx, first.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := s.PKICommitIssue(ctx, pkiID, fresh.Hash, "", pkiIssued("after-revoke")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		newToken, _ := pki.NewToken(pkiID, time.Hour)
		if err := s.PKICreateToken(ctx, newToken); err != nil {
			t.Fatal(err)
		}
	}
	extra, _ := pki.NewToken(pkiID, time.Hour)
	if err := s.PKICreateToken(ctx, extra); !errors.Is(err, pki.ErrConflict) {
		t.Fatal("bootstrap token limit not enforced")
	}
}
