package analysisai

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
)

type ResponsesProvider struct {
	config   Config
	client   *http.Client
	endpoint string
}

func NewResponsesProvider(cfg Config) *ResponsesProvider {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 45 * time.Second
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: timeout, DisableKeepAlives: true}
	return &ResponsesProvider{config: cfg, endpoint: Destination, client: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (p *ResponsesProvider) Analyze(ctx context.Context, preview Preview) (Result, error) {
	var result Result
	if !p.config.Enabled {
		return result, ErrDisabled
	}
	if preview.Request.Model != p.config.Model || preview.PromptVersion != PromptVersion {
		return result, &ProviderError{Code: "stale_provider_config"}
	}
	body, err := json.Marshal(preview.Request)
	if err != nil {
		return result, ErrInvalid
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != preview.Hash {
		return result, &ProviderError{Code: "preview_hash_mismatch"}
	}
	timeout := p.config.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := p.sendReviewed(bounded, body)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return result, &ProviderError{Code: "provider_rejected", Uncertain: response.StatusCode >= 500}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return result, &ProviderError{Code: "invalid_provider_response"}
	}
	var envelope struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Status != "completed" {
		return result, &ProviderError{Code: "provider_incomplete"}
	}
	text := ""
	for _, item := range envelope.Output {
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		for _, content := range item.Content {
			if content.Type == "refusal" {
				return result, &ProviderError{Code: "provider_refusal"}
			}
			if content.Type == "output_text" {
				if text != "" {
					return result, &ProviderError{Code: "multiple_output_messages"}
				}
				text = content.Text
			}
		}
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil {
		return result, &ProviderError{Code: "invalid_structured_result"}
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return result, &ProviderError{Code: "invalid_structured_result"}
	}
	if err = ValidateResult(&result, preview.Payload); err != nil {
		return Result{}, &ProviderError{Code: "invalid_evidence_references"}
	}
	return result, nil
}
func ValidateResult(result *Result, payload Payload) error {
	refs := map[string]bool{}
	for _, fact := range payload.Facts {
		refs[fact.Ref] = true
	}
	for _, relation := range payload.Relations {
		if relation.Ref != "" {
			refs[relation.Ref] = true
		}
	}
	if payload.LogSummaryRef == "logs_summary" && (len(payload.LogSeverityCounts) > 0 || len(payload.LogEventCounts) > 0) {
		refs[payload.LogSummaryRef] = true
	}
	clean := func(value *string, max int) bool {
		if len(*value) == 0 || len(*value) > max {
			return false
		}
		*value = logbook.SanitizeText(*value)
		return true
	}
	check := func(text *string, evidence []string) bool {
		if !clean(text, 2000) || len(evidence) < 1 || len(evidence) > 8 {
			return false
		}
		for _, ref := range evidence {
			if !refs[ref] {
				return false
			}
		}
		return true
	}
	if !clean(&result.Summary, 2000) || len(result.Observations) < 1 || len(result.Observations) > 10 || len(result.Hypotheses) > 10 || len(result.Recommendations) > 10 || len(result.Limitations) < 1 || len(result.Limitations) > 10 {
		return ErrInvalid
	}
	for i := range result.Observations {
		v := &result.Observations[i]
		if !check(&v.Text, v.Refs) {
			return ErrInvalid
		}
	}
	for i := range result.Hypotheses {
		v := &result.Hypotheses[i]
		if !check(&v.Text, v.Refs) || v.Confidence != "low" && v.Confidence != "medium" {
			return ErrInvalid
		}
	}
	for i := range result.Recommendations {
		v := &result.Recommendations[i]
		if !check(&v.Text, v.Refs) || v.Safety != "read_only" && v.Safety != "requires_operator_review" {
			return ErrInvalid
		}
	}
	for i := range result.Limitations {
		if !clean(&result.Limitations[i], 2000) {
			return ErrInvalid
		}
	}
	return nil
}
