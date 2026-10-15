package analysisai

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProviderExplicitRateLimitRetryAndUncertainNoRetry(t *testing.T) {
	for _, scenario := range []string{"rate", "quota", "server", "long-delay"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if scenario == "server" {
					w.WriteHeader(503)
					return
				}
				if calls == 1 || scenario == "quota" || scenario == "long-delay" {
					code := "rate_limit_exceeded"
					if scenario == "quota" {
						code = "insufficient_quota"
					}
					if scenario == "long-delay" {
						w.Header().Set("Retry-After", "60")
					}
					w.WriteHeader(429)
					json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "rate_limit_error", "code": code}})
					return
				}
				result, _ := json.Marshal(goodResult())
				json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(result)}}}}})
			}))
			defer server.Close()
			provider := NewResponsesProvider(Config{Enabled: true, Model: "fixture-model", APIKey: "test-key", Timeout: time.Second})
			provider.endpoint = server.URL
			provider.client = server.Client()
			_, err := provider.Analyze(t.Context(), fixturePreview(t))
			if scenario == "rate" {
				if err != nil || calls != 2 {
					t.Fatal("bounded retry", calls, err)
				}
			} else {
				if err == nil || calls != 1 {
					t.Fatal("unsafe repeat", calls, err)
				}
			}
			if scenario == "server" {
				var outcome *ProviderError
				if !errors.As(err, &outcome) || !outcome.Uncertain {
					t.Fatal("unknown outcome declared certain", err)
				}
			}
		})
	}
}
