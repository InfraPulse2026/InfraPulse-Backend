package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
)

type assetDecisionStub struct {
	assetStub
	writes  int
	failure error
	replay  bool
}

func (s *assetDecisionStub) AssetResolutionEvents(context.Context, string, int, int) ([]assets.ResolutionEvent, error) {
	return []assets.ResolutionEvent{}, nil
}

func (s *assetDecisionStub) MergeAssets(_ context.Context, in assets.MergeInput, _ string) (assets.Decision, bool, error) {
	s.writes++
	if !assets.ValidMerge(in) {
		return assets.Decision{}, false, assets.ErrInvalid
	}
	return assets.Decision{ID: "decision"}, s.replay, s.failure
}
func (s *assetDecisionStub) SplitAsset(_ context.Context, in assets.SplitInput, _ string) (assets.Decision, bool, error) {
	s.writes++
	if !assets.ValidSplit(in) {
		return assets.Decision{}, false, assets.ErrInvalid
	}
	return assets.Decision{}, s.replay, s.failure
}
func (s *assetDecisionStub) AssetCandidates(context.Context, string, int) ([]assets.Candidate, error) {
	return []assets.Candidate{}, nil
}
func (s *assetDecisionStub) AssetDecisions(context.Context, string, int, int) ([]assets.Decision, error) {
	return []assets.Decision{}, nil
}

func TestAssetDecisionsHTTPRolesConfirmationKeysAndReplay(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "fixture", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &assetDecisionStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})), WithAssets(repo))
			body := `{"target_id":"a","target_revision":1,"source_id":"b","source_revision":1,"canonical_binding_id":"binding","confirmed":true,"reason":"verified labels"}`
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/api/assets/merge", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "merge-key")
			h.ServeHTTP(w, r)
			want := 403
			if role == "admin" {
				want = 200
			}
			if w.Code != want {
				t.Fatal("merge role", w.Code, w.Body.String())
			}
			if role != "admin" && repo.writes != 0 {
				t.Fatal("forbidden merge reached storage")
			}
			for _, path := range []string{"/api/assets/a/candidates", "/api/assets/a/decisions"} {
				w = httptest.NewRecorder()
				r = httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatal("review read denied", w.Code)
				}
			}
			if role != "admin" {
				return
			}
			for _, key := range []string{"", "valid-key"} {
				w = httptest.NewRecorder()
				r = httptest.NewRequest("POST", "/api/assets/merge", strings.NewReader(strings.Replace(body, `"confirmed":true`, `"confirmed":false`, 1)))
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Content-Type", "application/json")
				if key != "" {
					r.Header.Set("Idempotency-Key", key)
				}
				h.ServeHTTP(w, r)
				if w.Code != 400 {
					t.Fatal("missing confirmation/key accepted", w.Code)
				}
			}
			repo.replay = true
			w = httptest.NewRecorder()
			r = httptest.NewRequest("POST", "/api/assets/merge", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "merge-key")
			h.ServeHTTP(w, r)
			if w.Header().Get("Idempotent-Replay") != "true" {
				t.Fatal("replay header")
			}
			repo.failure = assets.ErrConflict
			w = httptest.NewRecorder()
			r = httptest.NewRequest("POST", "/api/assets/merge", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "merge-key")
			h.ServeHTTP(w, r)
			if w.Code != 409 {
				t.Fatal("conflict contract", w.Code)
			}
		})
	}
}
