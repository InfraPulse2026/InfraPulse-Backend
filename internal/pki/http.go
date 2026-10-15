package pki

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Handler is served on a dedicated TLS listener with VerifyClientCertIfGiven:
// enrollment uses a bootstrap token, renewal requires a verified client leaf.
func (s *Service) Handler() http.Handler {
	var mu sync.Mutex
	window := time.Now()
	count := 0
	slots := make(chan struct{}, 8)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fail := func(code int) { http.Error(w, http.StatusText(code), code) }
		if r.TLS == nil {
			fail(400)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			fail(405)
			return
		}
		if r.URL.Path != "/enroll" && r.URL.Path != "/renew" {
			fail(404)
			return
		}
		if r.URL.RawQuery != "" {
			fail(400)
			return
		}
		mu.Lock()
		if time.Since(window) >= time.Minute {
			window = time.Now()
			count = 0
		}
		admitted := count < 120
		if admitted {
			count++
		}
		mu.Unlock()
		if !admitted {
			w.Header().Set("Retry-After", "60")
			fail(429)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			fail(429)
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			fail(415)
			return
		}
		var input struct {
			AgentID string `json:"agent_id"`
			CSR     []byte `json:"csr"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&input); err != nil {
			fail(400)
			return
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			fail(400)
			return
		}
		if _, err = AgentURI(input.AgentID); err != nil {
			fail(400)
			return
		}
		var hash []byte
		previous := ""
		if r.URL.Path == "/enroll" {
			values := r.Header.Values("Authorization")
			if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ip_enroll_") || len(values[0]) != len("Bearer ip_enroll_")+64 {
				fail(401)
				return
			}
			digest := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
			hash = digest[:]
		} else {
			if len(r.Header.Values("Authorization")) != 0 || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 || len(r.TLS.PeerCertificates) == 0 {
				fail(401)
				return
			}
			leaf := r.TLS.VerifiedChains[0][0]
			if !leaf.Equal(r.TLS.PeerCertificates[0]) || VerifyIdentity(leaf, input.AgentID) != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
				fail(401)
				return
			}
			previous = Fingerprint(leaf)
		}
		issued, err := s.Issuer.Sign(input.AgentID, input.CSR, time.Now())
		if err != nil {
			fail(400)
			return
		}
		err = s.Repository.PKICommitIssue(r.Context(), input.AgentID, hash, previous, issued)
		if err != nil {
			switch {
			case errors.Is(err, ErrDenied):
				fail(401)
			case errors.Is(err, ErrConflict):
				fail(409)
			default:
				fail(503)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Only after the durable authorization/token/certificate transaction commits.
		_ = json.NewEncoder(w).Encode(issued)
	})
}
