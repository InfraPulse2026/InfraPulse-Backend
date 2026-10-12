package collectorconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
)

var ErrInvalid = errors.New("invalid managed Collector configuration")
var ErrConflict = errors.New("Collector configuration revision or ownership conflict")
var ErrDisabled = errors.New("managed Collector configuration disabled")

const MaxBytes = 1 << 20

func Identifier(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
			return false
		}
	}
	return true
}
func ValidText(s string, max int) bool { return text(s, max) }
func text(s string, max int) bool {
	if !utf8.ValidString(s) || len(s) > max {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func closed(m protoreflect.Message) bool {
	if !m.IsValid() || len(m.GetUnknown()) > 0 {
		return false
	}
	good := true
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.Kind() == protoreflect.MessageKind {
			if f.IsList() {
				l := v.List()
				for n := 0; n < l.Len(); n++ {
					good = good && closed(l.Get(n).Message())
				}
			} else {
				good = good && closed(v.Message())
			}
		}
		return good
	})
	return good
}
func duration(v *durationpb.Duration) time.Duration {
	if v == nil {
		return 0
	}
	return v.AsDuration()
}
func fill(v **durationpb.Duration, d time.Duration) {
	if *v == nil || ((*v).Seconds == 0 && (*v).Nanos == 0) {
		*v = durationpb.New(d)
	}
}
func interval(v *durationpb.Duration, min, max time.Duration, zero bool) bool {
	if v == nil {
		return zero
	}
	if v.CheckValid() != nil {
		return false
	}
	d := v.AsDuration()
	return zero && d == 0 || d >= min && d <= max
}

// These defaults and canonical bytes implement the frozen Collector 215d33a v1
// contract. They are independent of local administrator polling defaults.
func Normalize(in *pb.DesiredConfigSnapshot) (*pb.DesiredConfigSnapshot, error) {
	if in == nil || !closed(in.ProtoReflect()) || len(in.Targets) > 1024 || proto.Size(in) > MaxBytes {
		return nil, ErrInvalid
	}
	s := proto.Clone(in).(*pb.DesiredConfigSnapshot)
	sort.Slice(s.Targets, func(i, j int) bool { return s.Targets[i].GetTargetId() < s.Targets[j].GetTargetId() })
	for _, t := range s.Targets {
		if t == nil {
			return nil, ErrInvalid
		}
		if t.Polling == nil {
			t.Polling = &pb.ManagedTargetPolling{}
		}
		p := t.Polling
		switch t.Protocol {
		case "snmp", "routeros":
			fill(&p.FastInterval, 3*time.Second)
			fill(&p.MediumInterval, 30*time.Second)
			fill(&p.InventoryInterval, 5*time.Minute)
			fill(&p.TopologyInterval, 5*time.Minute)
			fill(&p.Timeout, 2*time.Second)
		case "redfish", "ipmi":
			fill(&p.MediumInterval, 30*time.Second)
			fill(&p.InventoryInterval, 5*time.Minute)
			fill(&p.Timeout, 30*time.Second)
		case "sip":
			fill(&p.FastInterval, 30*time.Second)
			fill(&p.Timeout, 2*time.Second)
		default:
			return nil, ErrInvalid
		}
		switch t.Protocol {
		case "snmp":
			if t.Options == nil {
				t.Options = &pb.ManagedTargetSpec_Snmp{Snmp: &pb.ManagedSNMPOptions{}}
			}
			v := t.GetSnmp()
			if v == nil {
				return nil, ErrInvalid
			}
			if v.Version == "" {
				v.Version = "v2c"
			}
			if v.Version == "v3" {
				if v.SecurityLevel == "" {
					v.SecurityLevel = "authPriv"
				}
				if v.AuthProtocol == "" {
					v.AuthProtocol = "SHA256"
				}
				if v.SecurityLevel == "authPriv" && v.PrivacyProtocol == "" {
					v.PrivacyProtocol = "AES"
				}
			}
			if v.Port == 0 {
				v.Port = 161
			}
		case "sip":
			if t.Options == nil {
				t.Options = &pb.ManagedTargetSpec_Sip{Sip: &pb.ManagedSIPOptions{}}
			}
			v := t.GetSip()
			if v == nil {
				return nil, ErrInvalid
			}
			if v.Port == 0 {
				v.Port = 5060
				if v.Transport == "tls" {
					v.Port = 5061
				}
			}
			if v.Transport == "" {
				v.Transport = "udp"
			}
			if v.FromUser == "" {
				v.FromUser = "infrapulse"
			}
			if v.FailureThreshold == 0 {
				v.FailureThreshold = 3
			}
			fill(&v.LatencyThreshold, 250*time.Millisecond)
		case "redfish":
			if t.Options == nil {
				t.Options = &pb.ManagedTargetSpec_Redfish{Redfish: &pb.ManagedRedfishOptions{}}
			}
			v := t.GetRedfish()
			if v == nil {
				return nil, ErrInvalid
			}
			if v.Auth == "" {
				v.Auth = "session"
			}
			fill(&v.RequestTimeout, 5*time.Second)
			if v.MaxResources == 0 {
				v.MaxResources = 128
			}
			if v.MaxPages == 0 {
				v.MaxPages = 8
			}
			if v.MaxRequests == 0 {
				v.MaxRequests = 512
			}
		case "ipmi":
			if t.Options == nil {
				t.Options = &pb.ManagedTargetSpec_Ipmi{Ipmi: &pb.ManagedIPMIOptions{}}
			}
			v := t.GetIpmi()
			if v == nil {
				return nil, ErrInvalid
			}
			if v.Port == nil {
				v.Port = proto.Uint32(623)
			}
			if v.CipherSuite == nil {
				v.CipherSuite = proto.Uint32(17)
			}
			if v.Privilege == "" {
				v.Privilege = "user"
			}
			fill(&v.RequestTimeout, 2*time.Second)
			fill(&v.MaxSessionDuration, 30*time.Second)
			fill(&v.SdrCacheTtl, 5*time.Minute)
			for _, f := range []struct {
				v *int32
				d int32
			}{{&v.MaxOperations, 1024}, {&v.MaxSdrRecords, 128}, {&v.MaxSdrBytes, 128 << 10}, {&v.MaxFruRecords, 16}, {&v.MaxFruBytes, 64 << 10}, {&v.MaxPollBytes, 1 << 20}} {
				if *f.v == 0 {
					*f.v = f.d
				}
			}
		case "routeros":
			if t.Options != nil {
				return nil, ErrInvalid
			}
		}
	}
	return s, nil
}
func host(s string) bool {
	if s == "" || len(s) > 253 || strings.ContainsAny(s, "/@?#\\ \t\r\n\x00[]%") {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	if strings.Contains(s, ":") {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func Validate(s *pb.DesiredConfigSnapshot) error {
	if s == nil || !Identifier(s.CollectorId) || len(s.Targets) > 1024 || proto.Size(s) > MaxBytes {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, t := range s.Targets {
		if t == nil || !Identifier(t.TargetId) || seen[t.TargetId] || !text(t.Name, 128) || !text(t.VendorHint, 128) || t.CaRef != "" && !Identifier(t.CaRef) {
			return ErrInvalid
		}
		seen[t.TargetId] = true
		if t.Protocol == "sip" {
			if t.CredentialRef != "" {
				return ErrInvalid
			}
		} else if !Identifier(t.CredentialRef) {
			return ErrInvalid
		}
		l := t.GetLocation()
		for _, v := range []string{l.GetSite(), l.GetDatacenter(), l.GetBuilding(), l.GetRoom(), l.GetRack(), l.GetPosition()} {
			if !text(v, 128) {
				return ErrInvalid
			}
		}
		if !text(l.GetComment(), 1024) || l.GetRackUnit() > 100 {
			return ErrInvalid
		}
		if t.Protocol == "routeros" || t.Protocol == "redfish" {
			u, e := url.Parse(t.Endpoint)
			if e != nil || u.Scheme != "https" || u.Host == "" || !host(u.Hostname()) || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || !text(t.Endpoint, 2048) || strings.Contains(t.Endpoint, "\\") {
				return ErrInvalid
			}
			if u.Port() != "" {
				port, portErr := net.LookupPort("tcp", u.Port())
				if portErr != nil || port < 1 {
					return ErrInvalid
				}
			}
		} else if !host(t.Endpoint) {
			return ErrInvalid
		}
		p := t.GetPolling()
		min := 3 * time.Second
		if t.Protocol == "redfish" || t.Protocol == "ipmi" {
			min = 10 * time.Second
		}
		for _, v := range []*durationpb.Duration{p.GetFastInterval(), p.GetMediumInterval(), p.GetInventoryInterval(), p.GetTopologyInterval()} {
			if !interval(v, min, 24*time.Hour, true) {
				return ErrInvalid
			}
		}
		if !interval(p.GetTimeout(), 10*time.Millisecond, 5*time.Minute, false) {
			return ErrInvalid
		}
		switch t.Protocol {
		case "snmp":
			v := t.GetSnmp()
			if v == nil || v.Port < 1 || v.Port > 65535 || v.Retries < 0 || v.Retries > 10 {
				return ErrInvalid
			}
			if v.Version == "v2c" {
				if v.SecurityLevel != "" || v.AuthProtocol != "" || v.PrivacyProtocol != "" || v.ContextName != "" {
					return ErrInvalid
				}
			} else if v.Version == "v3" {
				if v.SecurityLevel != "authPriv" && v.SecurityLevel != "authNoPriv" || !text(v.ContextName, 32) || !oneOf(v.AuthProtocol, "MD5", "SHA", "SHA224", "SHA256", "SHA384", "SHA512") || v.SecurityLevel == "authNoPriv" && v.PrivacyProtocol != "" || v.SecurityLevel == "authPriv" && !oneOf(v.PrivacyProtocol, "DES", "AES", "AES192", "AES256", "AES192C", "AES256C") {
					return ErrInvalid
				}
			} else {
				return ErrInvalid
			}
			if t.CaRef != "" {
				return ErrInvalid
			}
		case "sip":
			v := t.GetSip()
			if v == nil || v.Port < 1 || v.Port > 65535 || !oneOf(v.Transport, "udp", "tcp", "tls") || !Identifier(v.FromUser) || v.ToUser != "" && !Identifier(v.ToUser) || v.Domain != "" && !host(v.Domain) || v.ServerName != "" && !host(v.ServerName) || v.FailureThreshold < 1 || v.FailureThreshold > 100 || !interval(v.LatencyThreshold, time.Millisecond, time.Minute, false) || !interval(p.Timeout, 10*time.Millisecond, time.Minute, false) || duration(p.MediumInterval) != 0 || duration(p.InventoryInterval) != 0 || duration(p.TopologyInterval) != 0 || v.Transport != "tls" && (t.CaRef != "" || v.ServerName != "") {
				return ErrInvalid
			}
		case "redfish":
			v := t.GetRedfish()
			if v == nil || !oneOf(v.Auth, "session", "basic") || !interval(v.RequestTimeout, 10*time.Millisecond, 30*time.Second, false) || v.MaxResources < 1 || v.MaxResources > 1024 || v.MaxPages < 1 || v.MaxPages > 64 || v.MaxRequests < 1 || v.MaxRequests > 4096 || duration(p.FastInterval) != 0 || duration(p.TopologyInterval) != 0 {
				return ErrInvalid
			}
		case "ipmi":
			v := t.GetIpmi()
			if v == nil || v.GetPort() < 1 || v.GetPort() > 65535 || !oneOfInt(v.GetCipherSuite(), 3, 17) || !oneOf(v.Privilege, "user", "operator") || v.Retries < 0 || v.Retries > 3 || !interval(v.RequestTimeout, 10*time.Millisecond, 30*time.Second, false) || !interval(v.MaxSessionDuration, 10*time.Millisecond, 5*time.Minute, false) || !interval(v.SdrCacheTtl, time.Second, time.Hour, false) || v.MaxOperations < 1 || v.MaxOperations > 8192 || v.MaxSdrRecords < 1 || v.MaxSdrRecords > 1024 || v.MaxSdrBytes < 260 || v.MaxSdrBytes > 1<<20 || v.MaxFruRecords < 1 || v.MaxFruRecords > 128 || v.MaxFruBytes < 8 || v.MaxFruBytes > 64<<10 || v.MaxPollBytes < v.MaxFruBytes || v.MaxPollBytes < v.MaxSdrBytes || v.MaxPollBytes > 8<<20 || duration(p.FastInterval) != 0 || duration(p.TopologyInterval) != 0 || t.CaRef != "" {
				return ErrInvalid
			}
		case "routeros":
			if t.Options != nil {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	}
	return nil
}
func oneOf(s string, vs ...string) bool {
	for _, v := range vs {
		if v == s {
			return true
		}
	}
	return false
}
func oneOfInt(s uint32, vs ...uint32) bool {
	for _, v := range vs {
		if v == s {
			return true
		}
	}
	return false
}

// Canonical encoding is the documented Agent contract, not protobuf/JSON bytes.
func CanonicalBytes(in *pb.DesiredConfigSnapshot) ([]byte, error) {
	s, e := Normalize(in)
	if e != nil {
		return nil, e
	}
	var b bytes.Buffer
	b.WriteString("InfraPulse managed config v1\n")
	str := func(v string) { _ = binary.Write(&b, binary.BigEndian, uint64(len(v))); b.WriteString(v) }
	num := func(v int64) { _ = binary.Write(&b, binary.BigEndian, v) }
	str(s.CollectorId)
	num(int64(len(s.Targets)))
	for _, t := range s.Targets {
		for _, v := range []string{t.TargetId, t.Name, t.Protocol, t.Endpoint, t.CredentialRef, t.VendorHint, t.CaRef} {
			str(v)
		}
		if t.Enabled {
			b.WriteByte(1)
		} else {
			b.WriteByte(0)
		}
		l := t.GetLocation()
		for _, v := range []string{l.GetSite(), l.GetDatacenter(), l.GetBuilding(), l.GetRoom(), l.GetRack(), l.GetPosition(), l.GetComment()} {
			str(v)
		}
		num(int64(l.GetRackUnit()))
		p := t.Polling
		for _, v := range []*durationpb.Duration{p.FastInterval, p.MediumInterval, p.InventoryInterval, p.TopologyInterval, p.Timeout} {
			num(int64(duration(v)))
		}
		for _, v := range []bool{t.GetSnmp() != nil, t.GetSip() != nil, t.GetRedfish() != nil, t.GetIpmi() != nil} {
			if v {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
		}
		if v := t.GetSnmp(); v != nil {
			for _, x := range []string{v.Version, v.SecurityLevel, v.AuthProtocol, v.PrivacyProtocol, v.ContextName} {
				str(x)
			}
			num(int64(v.Port))
			num(int64(v.Retries))
		}
		if v := t.GetSip(); v != nil {
			for _, x := range []string{v.Transport, v.FromUser, v.ToUser, v.Domain, v.ServerName} {
				str(x)
			}
			num(int64(v.Port))
			num(int64(v.FailureThreshold))
			num(int64(duration(v.LatencyThreshold)))
		}
		if v := t.GetRedfish(); v != nil {
			str(v.Auth)
			for _, x := range []int64{int64(duration(v.RequestTimeout)), int64(v.MaxResources), int64(v.MaxPages), int64(v.MaxRequests)} {
				num(x)
			}
		}
		if v := t.GetIpmi(); v != nil {
			num(int64(v.GetPort()))
			num(int64(v.GetCipherSuite()))
			str(v.Privilege)
			for _, x := range []int64{int64(v.Retries), int64(duration(v.RequestTimeout)), int64(duration(v.MaxSessionDuration)), int64(v.MaxOperations), int64(v.MaxSdrRecords), int64(v.MaxSdrBytes), int64(v.MaxFruRecords), int64(v.MaxFruBytes), int64(v.MaxPollBytes), int64(duration(v.SdrCacheTtl))} {
				num(x)
			}
		}
	}
	return b.Bytes(), nil
}
func Digest(s *pb.DesiredConfigSnapshot) (string, error) {
	b, e := CanonicalBytes(s)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
