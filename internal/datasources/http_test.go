package datasources

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func sourceTLSServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Source transport fixture CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "server.example"}, DNSNames: []string{"server.example"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}
func sourceHTTPFixture(t *testing.T, endpoint string, ca []byte) (*NetworkPolicy, Source) {
	t.Helper()
	p, err := NewNetworkPolicy(NetworkConfig{AllowedOrigins: []string{endpoint}, AllowedCIDRs: []string{"127.0.0.0/8"}, AllowLoopback: true, CABundles: map[string][]byte{"lab": ca}})
	if err != nil {
		t.Fatal(err)
	}
	return p, Source{Type: "PROMETHEUS", Endpoint: endpoint, Enabled: true, Configuration: Settings{TimeoutSeconds: 1, TLSCARef: "lab"}}
}

func TestSourceHTTPPrivateCAAndNoEnvironmentProxy(t *testing.T) {
	var calls atomic.Int32
	server, ca := sourceTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.URL.Path != "/prometheus/api/v1/query" || r.URL.Query().Get("query") != "up" {
			t.Error("request changed")
		}
		_, _ = io.WriteString(w, `{"status":"success"}`)
	})
	p, source := sourceHTTPFixture(t, server.URL, ca)
	source.Endpoint += "/prometheus"
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	client, err := NewSourceHTTP(p, source)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Do(t.Context(), HTTPRequest{Method: "GET", Path: "api/v1/query", Query: url.Values{"query": {"up"}}, Bearer: []byte("fixture-token")})
	if err != nil || response.StatusCode != 200 || calls.Load() != 1 {
		t.Fatal("private CA/proxy policy", err, calls.Load())
	}
	source.Configuration.TLSCARef = ""
	untrusted, err := NewSourceHTTP(p, source)
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.Close()
	_, err = untrusted.Do(t.Context(), HTTPRequest{Method: "GET", Bearer: []byte("fixture-token")})
	code, _ := Outcome(err)
	if code != "tls_failed" || calls.Load() != 1 {
		t.Fatal("untrusted certificate accepted or token transmitted", code, calls.Load())
	}
}

func TestSourceHTTPDNSRebindingAndMixedAnswers(t *testing.T) {
	var calls atomic.Int32
	server, ca := sourceTLSServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, `{}`) })
	u, _ := url.Parse(server.URL)
	endpoint := "https://server.example:" + u.Port()
	p, source := sourceHTTPFixture(t, endpoint, ca)
	var lookups atomic.Int32
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	})
	client, err := newSourceHTTP(p, source, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Do(t.Context(), HTTPRequest{Method: "GET"}); err != nil {
		t.Fatal("allowed DNS result", err)
	}
	client.Close()
	_, err = client.Do(t.Context(), HTTPRequest{Method: "GET"})
	code, _ := Outcome(err)
	if code != "network_policy_rejected" || calls.Load() != 1 || lookups.Load() != 2 {
		t.Fatal("rebinding escaped", code, calls.Load(), lookups.Load())
	}
	mixed := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("169.254.169.254")}, nil
	})
	client2, err := newSourceHTTP(p, source, mixed)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()
	_, err = client2.Do(t.Context(), HTTPRequest{Method: "GET"})
	code, _ = Outcome(err)
	if code != "network_policy_rejected" || calls.Load() != 1 {
		t.Fatal("mixed DNS answer permitted", code, calls.Load())
	}
}

func TestSourceHTTPRedirectBodyLimitCompressionAndDeadline(t *testing.T) {
	var redirected atomic.Int32
	target, ca := sourceTLSServer(t, func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) })
	server, ca2 := sourceTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, target.URL, 302)
		case "/large":
			w.Header().Set("Content-Length", strconv.Itoa(MaxSourceResponseBytes+1))
			w.WriteHeader(200)
		case "/chunked":
			w.(http.Flusher).Flush()
			chunk := strings.Repeat("x", 1<<20)
			for range 5 {
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
			}
		case "/compressed":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte{1, 2, 3})
		case "/slow":
			<-r.Context().Done()
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	_ = ca
	p, source := sourceHTTPFixture(t, server.URL, ca2)
	client, err := NewSourceHTTP(p, source)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, test := range []struct{ path, code string }{{"redirect", "redirect_blocked"}, {"large", "response_too_large"}, {"chunked", "response_too_large"}, {"compressed", "invalid_response"}} {
		_, err = client.Do(t.Context(), HTTPRequest{Method: "GET", Path: test.path, Bearer: []byte("fixture-token")})
		code, _ := Outcome(err)
		if code != test.code {
			t.Fatal(test.path, code, err)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect forwarded credentials")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = client.Do(ctx, HTTPRequest{Method: "GET", Path: "slow"})
	code, retry := Outcome(err)
	if code != "timeout" || !retry {
		t.Fatal("request ignored deadline", code)
	}
	for _, path := range []string{"https://other.example", "//other.example", "../admin", "api/%2e%2e/admin", "api\\admin"} {
		if _, err = client.Do(t.Context(), HTTPRequest{Method: "GET", Path: path}); err != ErrInvalid {
			t.Fatal("escaped adapter path", path, err)
		}
	}
}

func TestSourceHTTPChecksCertificateHostname(t *testing.T) {
	var calls atomic.Int32
	server, ca := sourceTLSServer(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	u, _ := url.Parse(server.URL)
	endpoint := "https://wrong.example:" + u.Port()
	p, source := sourceHTTPFixture(t, endpoint, ca)
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	client, err := newSourceHTTP(p, source, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Do(t.Context(), HTTPRequest{Method: "GET", Bearer: []byte("fixture-token")})
	code, _ := Outcome(err)
	if code != "tls_failed" || calls.Load() != 0 {
		t.Fatal("certificate hostname ignored", code, calls.Load())
	}
}
