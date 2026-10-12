// Package zabbix implements the read-only Zabbix JSON-RPC inventory adapter.
package zabbix

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
)

type transport interface {
	Do(context.Context, datasources.HTTPRequest) (datasources.HTTPResponse, error)
	Close()
}
type factory func(datasources.Source) (transport, error)

type Connector struct{ open factory }

// New always uses the deployment's SSRF policy and verified TLS transport.
func New(policy *datasources.NetworkPolicy) *Connector {
	return &Connector{open: func(source datasources.Source) (transport, error) {
		return datasources.NewSourceHTTP(policy, source)
	}}
}

func (c *Connector) Operations() []datasources.Operation {
	return []datasources.Operation{
		{Kind: "ZABBIX", Name: "test", ReadOnly: true, Run: c.test},
		{Kind: "ZABBIX", Name: "sync", ReadOnly: true, Run: c.sync},
	}
}

type client struct {
	http     transport
	token    []byte
	version  string
	legacy   bool
	sequence int
}

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_~+./=-]+$`)

func (c *Connector) connect(ctx context.Context, request datasources.Request) (*client, error) {
	if request.Source.Type != "ZABBIX" || len(request.Credential) < 16 || len(request.Credential) > 16384 || !tokenPattern.Match(request.Credential) {
		return nil, datasources.Failure{Code: "invalid_credentials"}
	}
	t, err := c.open(request.Source)
	if err != nil {
		return nil, err
	}
	rpc := &client{http: t, token: request.Credential}
	var version string
	// apiinfo.version must be unauthenticated, including on Zabbix 6.0.
	if err = rpc.call(ctx, "apiinfo.version", []string{}, false, &version); err != nil {
		t.Close()
		return nil, err
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		t.Close()
		return nil, datasources.Failure{Code: "unsupported_version"}
	}
	for _, part := range parts {
		if _, err = strconv.ParseUint(part, 10, 32); err != nil {
			t.Close()
			return nil, datasources.Failure{Code: "unsupported_version"}
		}
	}
	series := parts[0] + "." + parts[1]
	if series != "6.0" && series != "7.0" && series != "7.4" {
		t.Close()
		return nil, datasources.Failure{Code: "unsupported_version"}
	}
	rpc.version, rpc.legacy = version, series == "6.0"
	return rpc, nil
}

func (c *client) call(ctx context.Context, method string, params any, authenticated bool, out any) error {
	// The whitelist prevents retryable source jobs from issuing changes.
	if method != "apiinfo.version" && method != "host.get" && method != "item.get" && method != "problem.get" && method != "event.get" && method != "trigger.get" {
		return datasources.Failure{Code: "api_rejected"}
	}
	c.sequence++
	request := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
		ID      int    `json:"id"`
		Auth    string `json:"auth,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params, ID: c.sequence}
	if authenticated && c.legacy {
		request.Auth = string(c.token)
	}
	body, err := json.Marshal(request)
	if err != nil {
		return datasources.Failure{Code: "invalid_response"}
	}
	defer clear(body)
	httpRequest := datasources.HTTPRequest{Method: http.MethodPost, Body: body}
	if authenticated && !c.legacy {
		httpRequest.Bearer = c.token
	}
	response, err := c.http.Do(ctx, httpRequest)
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
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(response.Body, &envelope) != nil || envelope.JSONRPC != "2.0" || envelope.ID != c.sequence {
		return datasources.Failure{Code: "invalid_response"}
	}
	hasError := len(envelope.Error) != 0 && string(envelope.Error) != "null"
	if hasError {
		if len(envelope.Result) != 0 {
			return datasources.Failure{Code: "invalid_response"}
		}
		// Never reflect RPC message/data: remote errors can include secrets.
		return datasources.Failure{Code: "api_rejected"}
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" || json.Unmarshal(envelope.Result, out) != nil {
		return datasources.Failure{Code: "invalid_response"}
	}
	return nil
}

func (c *Connector) test(ctx context.Context, request datasources.Request) (datasources.Result, error) {
	rpc, err := c.connect(ctx, request)
	if err != nil {
		return datasources.Result{}, err
	}
	defer rpc.http.Close()
	var hosts []struct {
		HostID string `json:"hostid"`
	}
	err = rpc.call(ctx, "host.get", map[string]any{"output": []string{"hostid"}, "limit": 1}, true, &hosts)
	return datasources.Result{}, err
}
