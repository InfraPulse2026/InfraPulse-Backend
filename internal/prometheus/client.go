// Package prometheus implements bounded read-only Prometheus HTTP API access.
package prometheus

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
)

type transport interface {
	Do(context.Context, datasources.HTTPRequest) (datasources.HTTPResponse, error)
	Close()
}
type Connector struct {
	open  func(datasources.Source) (transport, error)
	slots chan struct{}
}

func New(policy *datasources.NetworkPolicy) *Connector {
	return &Connector{open: func(s datasources.Source) (transport, error) { return datasources.NewSourceHTTP(policy, s) }, slots: make(chan struct{}, 4)}
}

type client struct {
	http       transport
	source     datasources.Source
	cfg        prommodel.Settings
	credential []byte
	version    string
}

var bearerToken = regexp.MustCompile(`^[A-Za-z0-9_~+./=-]+$`)

func (c *Connector) acquire(ctx context.Context) error {
	select {
	case c.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Connector) release() { <-c.slots }
func (c *Connector) Operations() []datasources.Operation {
	return []datasources.Operation{{Kind: "PROMETHEUS", Name: "test", ReadOnly: true, Run: c.test}, {Kind: "PROMETHEUS", Name: "sync", ReadOnly: true, Run: c.sync}}
}
func (c *Connector) connect(ctx context.Context, s datasources.Source, credential []byte) (*client, error) {
	if s.Type != "PROMETHEUS" {
		return nil, datasources.Failure{Code: "configuration_required"}
	}
	cfg := prommodel.Settings{}
	if s.Configuration.Prometheus != nil {
		cfg = *s.Configuration.Prometheus
	}
	cfg, err := cfg.Normalize()
	if err != nil {
		return nil, datasources.Failure{Code: "configuration_required"}
	}
	switch cfg.AuthMode {
	case "none":
		if len(credential) > 0 {
			return nil, datasources.Failure{Code: "invalid_credentials"}
		}
	case "bearer":
		if len(credential) < 1 || len(credential) > 16384 || !bearerToken.Match(credential) {
			return nil, datasources.Failure{Code: "invalid_credentials"}
		}
	case "basic":
		if len(credential) == 0 || len(credential) > 16384 {
			return nil, datasources.Failure{Code: "invalid_credentials"}
		}
	}
	http, err := c.open(s)
	if err != nil {
		return nil, err
	}
	p := &client{http: http, source: s, cfg: cfg, credential: credential}
	var info struct {
		Version string `json:"version"`
	}
	if err = p.get(ctx, "api/v1/status/buildinfo", nil, &info); err != nil {
		http.Close()
		return nil, err
	}
	parts := strings.Split(info.Version, ".")
	if len(parts) != 3 || parts[0] != "3" {
		http.Close()
		return nil, datasources.Failure{Code: "unsupported_version"}
	}
	for _, part := range parts {
		if _, err = strconv.ParseUint(part, 10, 16); err != nil {
			http.Close()
			return nil, datasources.Failure{Code: "unsupported_version"}
		}
	}
	p.version = info.Version
	return p, nil
}
func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	switch path {
	case "api/v1/status/buildinfo", "api/v1/targets", "api/v1/query", "api/v1/query_range", "api/v1/targets/metadata":
	default:
		return datasources.Failure{Code: "api_rejected"}
	}
	r := datasources.HTTPRequest{Method: "GET", Path: path, Query: query}
	switch c.cfg.AuthMode {
	case "bearer":
		r.Bearer = c.credential
	case "basic":
		r.BasicUsername = c.cfg.Username
		r.BasicPassword = c.credential
	}
	response, err := c.http.Do(ctx, r)
	if err != nil {
		return err
	}
	defer clear(response.Body)
	switch {
	case response.StatusCode == 401 || response.StatusCode == 403:
		return datasources.Failure{Code: "invalid_credentials"}
	case response.StatusCode == 429:
		return datasources.Failure{Code: "rate_limited", Retryable: true}
	case response.StatusCode >= 500:
		return datasources.Failure{Code: "connection_failed", Retryable: true}
	case response.StatusCode != 200:
		return datasources.Failure{Code: "api_rejected"}
	}
	if len(response.Body) > datasources.MaxSourceResponseBytes {
		return datasources.Failure{Code: "response_too_large"}
	}
	var envelope struct {
		Status   string          `json:"status"`
		Data     json.RawMessage `json:"data"`
		Warnings []string        `json:"warnings"`
		Infos    []string        `json:"infos"`
	}
	if json.Unmarshal(response.Body, &envelope) != nil {
		return datasources.Failure{Code: "invalid_response"}
	}
	if envelope.Status == "error" {
		return datasources.Failure{Code: "api_rejected"}
	}
	if envelope.Status != "success" || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return datasources.Failure{Code: "invalid_response"}
	}
	// Warnings can mean partial results; do not promote these to a healthy sync.
	if len(envelope.Warnings) > 0 {
		return datasources.Failure{Code: "partial_response"}
	}
	if json.Unmarshal(envelope.Data, out) != nil {
		return datasources.Failure{Code: "invalid_response"}
	}
	return nil
}
func (c *Connector) test(ctx context.Context, r datasources.Request) (datasources.Result, error) {
	if err := c.acquire(ctx); err != nil {
		return datasources.Result{}, err
	}
	defer c.release()
	p, err := c.connect(ctx, r.Source, r.Credential)
	if err != nil {
		return datasources.Result{}, err
	}
	defer p.http.Close()
	return datasources.Result{}, nil
}
