package analysisai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/diagnosis"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
)

func fixturePreview(t *testing.T) Preview {
	t.Helper()
	now := time.Now()
	value := 85.0
	seed := health.Incident{ID: "private-incident", AgentID: "private-agent", DeviceID: "password=do-not-export", ResourceKind: "interface", ResourceID: "192.0.2.123", Source: "hostname-private", RuleID: "cpu.sustained", State: "open", Severity: "warning", OpenedAt: now.Add(-time.Minute), LastObservedAt: now, EvidenceStatus: "fresh", Signal: health.Assessment{Status: "warning", Value: &value}}
	payload, err := Export(diagnosis.Context{Seed: seed, Limitations: []string{"token=unsafe-text"}}, map[string]int64{"error": 3, "token=unsafe": 9})
	if err != nil {
		t.Fatal(err)
	}
	request, hash, err := BuildRequest("fixture-model", payload)
	if err != nil {
		t.Fatal(err)
	}
	return Preview{PromptVersion: PromptVersion, Request: request, Hash: hash, Payload: payload}
}
func goodResult() Result {
	return Result{Summary: "Есть подтверждённый сигнал нагрузки.", Observations: []Statement{{Text: "Зарегистрирован warning.", Refs: []string{"fact_1"}}}, Hypotheses: []Hypothesis{{Text: "Нужны дополнительные измерения.", Refs: []string{"fact_1"}, Confidence: "low"}}, Recommendations: []Recommendation{{Text: "Проверить свежие метрики.", Refs: []string{"fact_1"}, Safety: "read_only"}}, Limitations: []string{"Причина не доказана."}}
}
func TestExportAllowlistAndModelResponseValidation(t *testing.T) {
	preview := fixturePreview(t)
	raw, _ := json.Marshal(preview.Request)
	for _, secret := range []string{"private-agent", "private-incident", "do-not-export", "192.0.2.123", "hostname-private", "unsafe-text", "token=unsafe"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private/untrusted string escaped export")
		}
	}
	if preview.Request.Store || len(preview.Payload.Facts) != 1 || preview.Payload.Facts[0].Value == nil {
		t.Fatal("payload semantics wrong")
	}
	result := goodResult()
	if err := ValidateResult(&result, preview.Payload); err != nil {
		t.Fatal(err)
	}
	result.Observations[0].Refs = []string{"invented_fact"}
	if ValidateResult(&result, preview.Payload) == nil {
		t.Fatal("unknown reference accepted")
	}
	result = goodResult()
	result.Hypotheses[0].Confidence = "high"
	if ValidateResult(&result, preview.Payload) == nil {
		t.Fatal("unsupported causal confidence accepted")
	}
}
func TestResponsesProviderExactReviewedBodyAndNoTools(t *testing.T) {
	preview := fixturePreview(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		expected, _ := json.Marshal(preview.Request)
		if string(raw) != string(expected) || r.Header.Get("Authorization") != "Bearer test-key" || strings.Contains(string(raw), "test-key") {
			t.Error("provider body/header mismatch")
		}
		result, _ := json.Marshal(goodResult())
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(result)}}}}})
	}))
	defer server.Close()
	provider := NewResponsesProvider(Config{Enabled: true, Model: "fixture-model", APIKey: "test-key", Timeout: time.Second})
	provider.endpoint = server.URL
	provider.client = server.Client()
	if _, err := provider.Analyze(t.Context(), preview); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("automatic retry")
	}
	preview.Request.Input = "tampered"
	if _, err := provider.Analyze(t.Context(), preview); err == nil || calls != 1 {
		t.Fatal("modified reviewed payload sent")
	}
}
func TestProviderRedirectRefusalAndIncompleteRejected(t *testing.T) {
	preview := fixturePreview(t)
	for _, tc := range []struct {
		status   int
		response any
	}{{302, nil}, {200, map[string]any{"status": "incomplete"}}, {200, map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "refusal"}}}}}}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.status == 302 {
				w.Header().Set("Location", "http://127.0.0.1:1/should-not-follow")
			}
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(tc.response)
		}))
		provider := NewResponsesProvider(Config{Enabled: true, Model: "fixture-model", APIKey: "test-key", Timeout: time.Second})
		provider.endpoint = server.URL
		if _, err := provider.Analyze(context.Background(), preview); err == nil {
			t.Fatal("invalid provider outcome accepted")
		}
		server.Close()
	}
}
