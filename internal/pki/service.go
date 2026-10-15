package pki

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"time"
)

var ErrDenied = errors.New("PKI credential rejected")
var ErrConflict = errors.New("PKI operation conflicts with current state")
var ErrNotFound = errors.New("PKI resource not found")

type Config struct {
	Enabled   bool
	Address   string
	CAFile    string
	CAKeyFile string
	CertFile  string
	KeyFile   string
	Lifetime  time.Duration
}

type Token struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agent_id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	Secret    string     `json:"token,omitempty"`
	Hash      []byte     `json:"-"`
}
type Certificate struct {
	Fingerprint string     `json:"fingerprint"`
	AgentID     string     `json:"agent_id"`
	Serial      string     `json:"serial"`
	NotBefore   time.Time  `json:"not_before"`
	NotAfter    time.Time  `json:"not_after"`
	CreatedAt   time.Time  `json:"created_at"`
	RenewedFrom *string    `json:"renewed_from"`
	RevokedAt   *time.Time `json:"revoked_at"`
}
type Audit struct {
	ID       int64     `json:"id"`
	AgentID  string    `json:"agent_id"`
	ActorID  *string   `json:"actor_id"`
	Action   string    `json:"action"`
	Resource string    `json:"resource"`
	At       time.Time `json:"at"`
}
type Repository interface {
	PKICreateToken(context.Context, Token) error
	PKITokens(context.Context, string, int) ([]Token, error)
	PKIRevokeToken(context.Context, string) error
	PKICommitIssue(context.Context, string, []byte, string, Issued) error
	PKICertificateActive(context.Context, string, string) error
	PKICertificates(context.Context, string, int) ([]Certificate, error)
	PKIRevokeCertificate(context.Context, string) (string, error)
	PKIAudit(context.Context, string, int) ([]Audit, error)
}
type Service struct {
	Repository Repository
	Issuer     *Issuer
	OnRevoke   func(string)
}

func NewToken(id string, ttl time.Duration) (Token, error) {
	var token Token
	if _, err := AgentURI(id); err != nil {
		return token, err
	}
	if ttl < time.Minute || ttl > 24*time.Hour {
		return token, errors.New("token TTL must be 1m..24h")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return token, err
	}
	token.Secret = "ip_enroll_" + hex.EncodeToString(b)
	hash := sha256.Sum256([]byte(token.Secret))
	token.Hash = hash[:]
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return Token{}, err
	}
	token.ID = hex.EncodeToString(idBytes)
	token.AgentID = id
	token.CreatedAt = time.Now().UTC()
	token.ExpiresAt = token.CreatedAt.Add(ttl)
	return token, nil
}

func Fingerprint(cert *x509.Certificate) string {
	h := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(h[:])
}

func (s *Service) AuthorizePeer(ctx context.Context, id string) error {
	if err := AuthorizePeer(ctx, id); err != nil {
		return err
	}
	leaf, err := peerCertificate(ctx)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.Repository.PKICertificateActive(bounded, id, Fingerprint(leaf))
}
