package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	rm "github.com/InfraPulse2026/InfraPulse-Backend/internal/remediation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
)

type remediationAPIStub struct {
	calls   int
	err     error
	approve rm.ApproveInput
}

func (s *remediationAPIStub) CreateRemediation(context.Context, string, rm.CreateInput, string) (rm.Proposal, bool, error) {
	s.calls++
	return rm.Proposal{ID: "proposal_one", State: "proposed"}, true, s.err
}
func (s *remediationAPIStub) Remediation(context.Context, string) (rm.Proposal, error) {
	s.calls++
	return rm.Proposal{ID: "proposal_one", State: "queued", JobID: "job_one"}, s.err
}
func (s *remediationAPIStub) ApproveRemediation(_ context.Context, _ string, in rm.ApproveInput) (rm.Proposal, bool, error) {
	s.calls++
	s.approve = in
	return rm.Proposal{ID: "proposal_one", State: "queued", JobID: "job_one"}, true, s.err
}
func (s *remediationAPIStub) RejectRemediation(context.Context, string, rm.RejectInput) (rm.Proposal, error) {
	s.calls++
	return rm.Proposal{ID: "proposal_one", State: "rejected"}, s.err
}
func (s *remediationAPIStub) ReconcileRemediation(context.Context, string, rm.ReconcileInput) (rm.Proposal, error) {
	s.calls++
	return rm.Proposal{ID: "proposal_one", State: "reconciled", AfterSnapshotID: "snapshot_one"}, s.err
}
func (s *remediationAPIStub) RemediationHistory(context.Context, string, int, int) ([]rm.HistoryEvent, error) {
	s.calls++
	return nil, s.err
}

func remediationHandler(t *testing.T, role string, repo *remediationAPIStub) (http.Handler, string) {
	t.Helper()
	token, err := access.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := access.TokenHash(token)
	identity := accessStub{principal: access.Principal{User: access.User{ID: "user", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
	return New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithRemediation(repo), WithAccess(access.New(identity, access.Config{Mode: "required", SessionTTL: time.Hour}))), token
}

func remediationRequest(h http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "remediation-request-one")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRemediationRoutesAuthenticationRolesAndAsyncApproval(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			repo := &remediationAPIStub{}
			h, token := remediationHandler(t, role, repo)
			for _, route := range []struct {
				method, path, body string
				success            int
			}{
				{"GET", "/api/remediations/proposal_one", "", 200},
				{"GET", "/api/remediations/proposal_one/history", "", 200},
				{"POST", "/api/assets/asset_one/configuration/remediations", `{"drift_id":1,"reason":"reviewed VLAN drift"}`, 201},
				{"POST", "/api/remediations/proposal_one/approve", `{"confirmed":true,"digest":"` + digest + `","reason":"approved exact VLAN action"}`, 202},
				{"POST", "/api/remediations/proposal_one/reject", `{"reason":"rejected reviewed action"}`, 200},
				{"POST", "/api/remediations/proposal_one/reconcile", `{"snapshot_id":"snapshot_one","confirmed":true,"reason":"reviewed fresh actual state"}`, 200},
			} {
				before := repo.calls
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(route.method, route.path, strings.NewReader(route.body)))
				if w.Code != 401 || repo.calls != before {
					t.Fatal("anonymous reached remediation", w.Code)
				}
				w = remediationRequest(h, token, route.method, route.path, route.body)
				want := route.success
				if role == "viewer" && route.method == "POST" {
					want = 403
				}
				if w.Code != want {
					t.Fatalf("%s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
				}
				if want == 403 && repo.calls != before {
					t.Fatal("viewer reached remediation mutation")
				}
				if w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("missing no-store")
				}
				if want == 202 {
					var out rm.Proposal
					if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.State != "queued" || out.JobID != "job_one" || w.Header().Get("Location") != "/api/remediations/proposal_one" || w.Header().Get("Idempotent-Replay") != "true" {
						t.Fatal("approval falsely implies completion", w.Body.String())
					}
					if repo.approve.Digest != digest {
						t.Fatal("approval digest not bound")
					}
				}
			}
		})
	}
}

func TestRemediationClosedInputsAndCookieCSRF(t *testing.T) {
	repo := &remediationAPIStub{}
	h, token := remediationHandler(t, "operator", repo)
	digest := strings.Repeat("b", 64)
	valid := `{"confirmed":true,"digest":"` + digest + `","reason":"approved reviewed proposal"}`
	for _, body := range []string{`{"drift_id":1,"reason":"test","asset_id":"asset_two"}`, `{"drift_id":1,"reason":"test","interface":"Gi0/12"}`, `{"drift_id":1,"reason":"test","command":"reload"}`, `{"drift_id":0,"reason":"test"}`, `{"drift_id":1,"reason":"test","inventory":"*"}`} {
		before := repo.calls
		w := remediationRequest(h, token, "POST", "/api/assets/asset_one/configuration/remediations", body)
		if (w.Code != 400 && w.Code != 422) || repo.calls != before {
			t.Fatal("mutable proposal input reached storage", w.Code, w.Body.String())
		}
	}
	before := repo.calls
	r := httptest.NewRequest("POST", "/api/assets/asset_one/configuration/remediations", strings.NewReader(`{"drift_id":1,"reason":"reviewed proposal"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 422 || repo.calls != before {
		t.Fatal("proposal without idempotency key reached storage", w.Code)
	}
	for _, body := range []string{`{"confirmed":true,"digest":"` + digest + `","reason":"test","target_vlan":20}`, `{"confirmed":true,"digest":"` + digest + `","reason":"test","playbook":"../../evil.yml"}`, `{"confirmed":true,"digest":"` + digest + `","reason":"test","extra_vars":{}}`, `{"confirmed":false,"digest":"` + digest + `","reason":"test"}`, strings.Replace(valid, digest, "bad", 1), valid + ` {}`, strings.Repeat("x", 2049)} {
		before := repo.calls
		w := remediationRequest(h, token, "POST", "/api/remediations/proposal_one/approve", body)
		if (w.Code != 400 && w.Code != 422) || repo.calls != before {
			t.Fatal("untrusted action input reached storage", w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/remediations/proposal_one?extra_vars=x", "/api/remediations/proposal_one/history?limit=1&limit=2", "/api/remediations/proposal_one/history?offset=100001"} {
		before := repo.calls
		w := remediationRequest(h, token, "GET", path, "")
		if (w.Code != 400 && w.Code != 422) || repo.calls != before {
			t.Fatal("invalid query reached storage", w.Code)
		}
	}
	for _, tc := range []struct {
		csrf, origin string
		want         int
	}{
		{"", "", 403}, {"invalid", "", 403}, {access.CSRF(token), "https://other.example", 403}, {access.CSRF(token), "", 202},
	} {
		before := repo.calls
		r := httptest.NewRequest("POST", "/api/remediations/proposal_one/approve", strings.NewReader(valid))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: access.CookieName, Value: token})
		if tc.csrf != "" {
			r.Header.Set("X-CSRF-Token", tc.csrf)
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want || tc.want == 403 && repo.calls != before {
			t.Fatal("cookie gate", w.Code, w.Body.String())
		}
	}
}

func TestRemediationOpenAPIClosedRequestsAndLifecycle(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				AdditionalProperties *bool                      `json:"additionalProperties"`
				Properties           map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openapi, &spec); err != nil {
		t.Fatal(err)
	}
	for schema, want := range map[string][]string{"RemediationCreateInput": {"drift_id", "reason"}, "RemediationApproveInput": {"confirmed", "digest", "reason"}, "RemediationRejectInput": {"reason"}, "RemediationReconcileInput": {"snapshot_id", "confirmed", "reason"}} {
		v := spec.Components.Schemas[schema]
		if v.AdditionalProperties == nil || *v.AdditionalProperties || len(v.Properties) != len(want) {
			t.Fatal("request contract allows arbitrary action parameters", schema)
		}
		for _, name := range want {
			if _, ok := v.Properties[name]; !ok {
				t.Fatal("missing typed request field", schema, name)
			}
		}
	}
	if _, ok := spec.Paths["/api/remediations/{id}/approve"]["post"].Responses["202"]; !ok {
		t.Fatal("approval missing asynchronous response")
	}
	for _, field := range []string{"action", "digest", "state", "job_id", "verification", "actual_observed_at", "expected_observed_at", "warnings"} {
		if _, ok := spec.Components.Schemas["RemediationProposal"].Properties[field]; !ok {
			t.Fatal("preview/status/evidence incomplete", field)
		}
	}
}

func TestRemediationAPIErrorContractsAndHistory(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{rm.ErrInvalid, 422, "invalid_remediation_action"}, {rm.ErrConflict, 409, "remediation_conflict"}, {rm.ErrExpired, 409, "remediation_conflict"}, {rm.ErrPolicy, 403, "remediation_policy_denied"}, {rm.ErrUnavailable, 503, "remediation_unavailable"}, {storage.ErrNotFound, 404, "not_found"}, {access.ErrForbidden, 403, "forbidden"},
	} {
		repo := &remediationAPIStub{err: tc.err}
		h, token := remediationHandler(t, "operator", repo)
		w := remediationRequest(h, token, "GET", "/api/remediations/proposal_one", "")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatal(tc.err, w.Code, w.Body.String())
		}
	}
	repo := &remediationAPIStub{}
	h, token := remediationHandler(t, "viewer", repo)
	w := remediationRequest(h, token, "GET", "/api/remediations/proposal_one/history?limit=2&offset=1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) || !strings.Contains(w.Body.String(), `"limit":2`) || !strings.Contains(w.Body.String(), `"offset":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestRemediationReconcileClosedInputAndCSRF(t *testing.T) {
	repo := &remediationAPIStub{}
	h, token := remediationHandler(t, "operator", repo)
	path := "/api/remediations/proposal_one/reconcile"
	valid := `{"snapshot_id":"snapshot_one","confirmed":true,"reason":"reviewed fresh actual state"}`
	for _, body := range []string{
		`{"snapshot_id":"snapshot_one","confirmed":true,"reason":"test","target_vlan":20}`,
		`{"snapshot_id":"snapshot_one","confirmed":true,"reason":"test","playbook":"restore.yml"}`,
		`{"snapshot_id":"snapshot_one","confirmed":true,"reason":"test","extra_vars":{}}`,
		`{"snapshot_id":"../snapshot_one","confirmed":true,"reason":"test"}`,
		`{"snapshot_id":"snapshot_one","confirmed":false,"reason":"test"}`,
	} {
		before := repo.calls
		w := remediationRequest(h, token, "POST", path, body)
		if (w.Code != 400 && w.Code != 422) || repo.calls != before {
			t.Fatal("invalid reconciliation reached storage", w.Code, w.Body.String())
		}
	}
	for _, csrf := range []string{"", access.CSRF(token)} {
		before := repo.calls
		r := httptest.NewRequest("POST", path, strings.NewReader(valid))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: access.CookieName, Value: token})
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if csrf == "" {
			if w.Code != 403 || repo.calls != before {
				t.Fatal("reconcile CSRF gate bypassed", w.Code)
			}
		} else {
			var out rm.Proposal
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.State != "reconciled" || out.AfterSnapshotID != "snapshot_one" || out.JobID != "" {
				t.Fatal("reconcile falsely queues a write", w.Code, w.Body.String())
			}
		}
	}
}
