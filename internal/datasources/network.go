package datasources

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var ErrNetworkPolicy = errors.New("data source network policy rejected the destination")
var caRefPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// NetworkConfig is deployment configuration, never controlled by source REST input.
type NetworkConfig struct {
	AllowedOrigins []string
	AllowedCIDRs   []string
	AllowLoopback  bool
	CABundles      map[string][]byte
}

// NetworkPolicy is immutable. A hostname allowlist alone is insufficient: every
// resolved address must also be allowed. Link-local/metadata targets stay denied.
type NetworkPolicy struct {
	origins       map[string]bool
	prefixes      []netip.Prefix
	allowLoopback bool
	caPools       map[string]*x509.CertPool
}

var neverDial = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("100.100.100.200/32"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"),
}

func parseEndpoint(raw string) (*url.URL, string, error) {
	if raw == "" || !safeText(raw, 2048) || strings.ContainsAny(raw, "\\#") {
		return nil, "", ErrInvalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.Contains(u.Host, "%") {
		return nil, "", ErrInvalid
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip, e := netip.ParseAddr(host); e == nil {
		host = ip.Unmap().String()
	} else {
		if len(host) > 253 {
			return nil, "", ErrInvalid
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, "", ErrInvalid
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return nil, "", ErrInvalid
				}
			}
		}
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return nil, "", ErrInvalid
		}
		port = "443"
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return nil, "", ErrInvalid
	}
	port = strconv.Itoa(n)
	if !safePath(u.Path) {
		return nil, "", ErrInvalid
	}
	u.Host = net.JoinHostPort(host, port)
	return u, "https://" + u.Host, nil
}

func safePath(path string) bool {
	if strings.ContainsAny(path, "\\") || !safeText(path, 2048) {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." || part == "." {
			return false
		}
	}
	return true
}

func NewNetworkPolicy(c NetworkConfig) (*NetworkPolicy, error) {
	p := &NetworkPolicy{origins: map[string]bool{}, caPools: map[string]*x509.CertPool{}, allowLoopback: c.AllowLoopback}
	if len(c.AllowedOrigins) > 128 || len(c.AllowedCIDRs) > 128 || len(c.CABundles) > 32 {
		return nil, ErrInvalid
	}
	for _, raw := range c.AllowedOrigins {
		u, origin, err := parseEndpoint(raw)
		if err != nil || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("DATA_SOURCE_ALLOWED_ORIGINS must contain exact HTTPS origins without paths")
		}
		p.origins[origin] = true
	}
	for _, raw := range c.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Is4In6() {
			return nil, errors.New("DATA_SOURCE_ALLOWED_CIDRS must contain IPv4 or IPv6 CIDRs")
		}
		p.prefixes = append(p.prefixes, prefix.Masked())
	}
	for ref, bundle := range c.CABundles {
		if !caRefPattern.MatchString(ref) || len(bundle) == 0 || len(bundle) > 1<<20 {
			return nil, errors.New("invalid data source CA bundle")
		}
		pool := x509.NewCertPool()
		count := 0
		for len(strings.TrimSpace(string(bundle))) > 0 {
			if !strings.HasPrefix(strings.TrimSpace(string(bundle)), "-----BEGIN CERTIFICATE-----") {
				return nil, errors.New("data source CA bundles must contain CA certificates only")
			}
			block, rest := pem.Decode(bundle)
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				return nil, errors.New("data source CA bundles must contain CA certificates only")
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil || !cert.IsCA || !cert.BasicConstraintsValid {
				return nil, errors.New("data source CA bundle contains an invalid CA")
			}
			pool.AddCert(cert)
			count++
			bundle = rest
		}
		if count == 0 {
			return nil, errors.New("empty data source CA bundle")
		}
		p.caPools[ref] = pool
	}
	return p, nil
}

func (p *NetworkPolicy) CheckEndpoint(endpoint, caRef string) error {
	_, origin, err := parseEndpoint(endpoint)
	if err != nil {
		return ErrNetworkPolicy
	}
	if p == nil || !p.origins[origin] {
		return ErrNetworkPolicy
	}
	if caRef != "" {
		if _, ok := p.caPools[caRef]; !ok {
			return ErrNetworkPolicy
		}
	}
	return nil
}

func (p *NetworkPolicy) CheckAddress(ip netip.Addr) error {
	ip = ip.Unmap()
	if p == nil || !ip.IsValid() || ip.Zone() != "" || (!ip.IsGlobalUnicast() && !ip.IsLoopback()) || ip.IsLinkLocalUnicast() || (ip.IsLoopback() && !p.allowLoopback) {
		return ErrNetworkPolicy
	}
	for _, prefix := range neverDial {
		if prefix.Contains(ip) {
			return ErrNetworkPolicy
		}
	}
	for _, prefix := range p.prefixes {
		if prefix.Contains(ip) {
			return nil
		}
	}
	return ErrNetworkPolicy
}

func (p *NetworkPolicy) CARefs() []string {
	items := []string{}
	if p != nil {
		for ref := range p.caPools {
			items = append(items, ref)
		}
	}
	sort.Strings(items)
	return items
}

func SameEndpoint(a, b string) bool {
	if a == b {
		return true
	}
	u, _, err := parseEndpoint(a)
	if err != nil {
		return false
	}
	v, _, err := parseEndpoint(b)
	return err == nil && u.String() == v.String()
}
