package analysisai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

func (p *ResponsesProvider) sendReviewed(ctx context.Context, body []byte) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, ErrInvalid
		}
		request.Header.Set("Authorization", "Bearer "+p.config.APIKey)
		request.Header.Set("Content-Type", "application/json")
		response, err := p.client.Do(request)
		if err != nil {
			return nil, &ProviderError{Code: "transport_outcome_unknown", Uncertain: true}
		}
		if response.StatusCode != http.StatusTooManyRequests || attempt == 1 {
			return response, nil
		}
		raw, e := io.ReadAll(io.LimitReader(response.Body, 4097))
		response.Body.Close()
		var rejected struct{ Error struct{ Type, Code string } }
		if e != nil || len(raw) > 4096 || json.Unmarshal(raw, &rejected) != nil || rejected.Error.Code == "insufficient_quota" || (rejected.Error.Type != "rate_limit_error" && rejected.Error.Code != "rate_limit_exceeded" && rejected.Error.Code != "slow_down") {
			return nil, &ProviderError{Code: "provider_rate_limited"}
		}
		jitter := []byte{0}
		_, _ = rand.Read(jitter)
		delay := 250*time.Millisecond + time.Duration(jitter[0]%100)*time.Millisecond
		if header := response.Header.Get("Retry-After"); header != "" {
			if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
				if seconds > 60 {
					return nil, &ProviderError{Code: "provider_rate_limited"}
				}
				if requested := time.Duration(seconds) * time.Second; requested > delay {
					delay = requested
				}
			} else if at, err := http.ParseTime(header); err == nil {
				if requested := time.Until(at); requested > delay {
					delay = requested
				}
			} else {
				return nil, &ProviderError{Code: "provider_rate_limited"}
			}
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return nil, &ProviderError{Code: "provider_rate_limited"}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, &ProviderError{Code: "provider_rate_limited"}
		case <-timer.C:
		}
	}
	return nil, &ProviderError{Code: "provider_rate_limited"}
}
