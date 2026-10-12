package datasources

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const MaxSourceResponseBytes = 4 << 20
const MaxSourceRequestBytes = 1 << 20

type netResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// SourceHTTP confines requests to one configured origin and pins DNS results at
// the TCP dial. No environment proxy, redirects, cookie jar or adapter-level retry.
type SourceHTTP struct {
	base      *url.URL
	client    *http.Client
	transport *http.Transport
}

type HTTPRequest struct {
	Method string
	// Path is a trusted adapter-relative path. Empty addresses the exact endpoint.
	Path          string
	Query         url.Values
	Body          []byte
	Bearer        []byte
	BasicUsername string
	BasicPassword []byte
}
type HTTPResponse struct {
	StatusCode int
	Body       []byte `json:"-"`
}

func NewSourceHTTP(policy *NetworkPolicy, source Source) (*SourceHTTP, error) {
	return newSourceHTTP(policy, source, net.DefaultResolver)
}

func newSourceHTTP(policy *NetworkPolicy, source Source, resolver netResolver) (*SourceHTTP, error) {
	if !source.Enabled {
		return nil, ErrDisabled
	}
	if err := policy.CheckEndpoint(source.Endpoint, source.Configuration.TLSCARef); err != nil {
		return nil, err
	}
	base, _, err := parseEndpoint(source.Endpoint)
	if err != nil {
		return nil, err
	}
	if source.Configuration.TimeoutSeconds < 1 || source.Configuration.TimeoutSeconds > 120 || resolver == nil {
		return nil, ErrInvalid
	}
	host, port := base.Hostname(), base.Port()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if ref := source.Configuration.TLSCARef; ref != "" {
		tlsConfig.RootCAs = policy.caPools[ref].Clone()
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		Proxy: nil, Protocols: protocols, TLSClientConfig: tlsConfig,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		MaxResponseHeaderBytes: 32 << 10, DisableCompression: true,
		MaxIdleConns: 2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		actualHost, actualPort, e := net.SplitHostPort(address)
		if e != nil || actualHost != host || actualPort != port || network != "tcp" {
			return nil, ErrNetworkPolicy
		}
		addresses := []netip.Addr{}
		if ip, e := netip.ParseAddr(host); e == nil {
			addresses = append(addresses, ip)
		} else {
			addresses, e = resolver.LookupNetIP(ctx, "ip", host)
			if e != nil {
				return nil, Failure{Code: "connection_failed", Retryable: true}
			}
		}
		if len(addresses) == 0 || len(addresses) > 16 {
			return nil, ErrNetworkPolicy
		}
		// Fail the entire DNS answer if any address escapes policy. Never choose
		// a convenient allowed result while another result points to metadata.
		for _, ip := range addresses {
			if e = policy.CheckAddress(ip); e != nil {
				return nil, e
			}
		}
		dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		for _, ip := range addresses {
			conn, e := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
			if e == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, Failure{Code: "connection_failed", Retryable: true}
	}
	client := &http.Client{Transport: transport, Timeout: time.Duration(source.Configuration.TimeoutSeconds) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &SourceHTTP{base: base, client: client, transport: transport}, nil
}

func (c *SourceHTTP) Close() { c.transport.CloseIdleConnections() }

func (c *SourceHTTP) Do(ctx context.Context, input HTTPRequest) (HTTPResponse, error) {
	var empty HTTPResponse
	if c == nil || c.client == nil || input.Method != "GET" && input.Method != "POST" || len(input.Body) > MaxSourceRequestBytes || strings.HasPrefix(input.Path, "/") || strings.ContainsAny(input.Path, "%?#:") || !safePath(input.Path) || (len(input.Bearer) > 0 && (input.BasicUsername != "" || len(input.BasicPassword) > 0)) {
		return empty, ErrInvalid
	}
	if len(input.Bearer) > 16384 || !safeText(string(input.Bearer), 16384) || !safeText(input.BasicUsername, 256) || strings.Contains(input.BasicUsername, ":") || len(input.BasicPassword) > 16384 {
		return empty, ErrInvalid
	}
	u := *c.base
	if input.Path != "" {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + input.Path
		u.RawPath = ""
	}
	u.RawQuery = input.Query.Encode()
	if len(u.RawQuery) > 64<<10 {
		return empty, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, input.Method, u.String(), bytes.NewReader(input.Body))
	if err != nil {
		return empty, ErrInvalid
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if len(input.Bearer) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(input.Bearer))
	}
	if input.BasicUsername != "" || len(input.BasicPassword) > 0 {
		req.SetBasicAuth(input.BasicUsername, string(input.BasicPassword))
	}
	response, err := c.client.Do(req)
	if err != nil {
		return empty, sourceHTTPError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return empty, Failure{Code: "redirect_blocked"}
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return empty, Failure{Code: "invalid_response"}
	}
	if response.ContentLength > MaxSourceResponseBytes {
		return empty, Failure{Code: "response_too_large"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxSourceResponseBytes+1))
	if err != nil {
		return empty, sourceHTTPError(ctx, err)
	}
	if len(body) > MaxSourceResponseBytes {
		return empty, Failure{Code: "response_too_large"}
	}
	return HTTPResponse{StatusCode: response.StatusCode, Body: body}, nil
}

func sourceHTTPError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrNetworkPolicy) {
		return Failure{Code: "network_policy_rejected"}
	}
	var certErr *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var record tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &record) {
		return Failure{Code: "tls_failed"}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Failure{Code: "timeout", Retryable: true}
	}
	return Failure{Code: "connection_failed", Retryable: true}
}
