package telephony

import (
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math"
	"strings"
	"testing"
	"time"
)

func validUpdate(now time.Time) *pb.TelephonyUpdate {
	code := uint32(401)
	latency := 0.0
	return &pb.TelephonyUpdate{ObservedAt: timestamppb.New(now), Source: "sip.options", Targets: []*pb.TelephonyHealth{{TargetId: "pbx", Address: "pbx:5061", Transport: "tls", Reachable: true, ResponseCode: &code, LatencyMs: &latency, ObservedAt: timestamppb.New(now), State: "degraded", ErrorCategory: "authentication_required", Source: "sip.options"}}}
}
func TestValidateAgentSemanticsAndPresence(t *testing.T) {
	base := validUpdate(time.Now())
	if err := Validate(base); err != nil {
		t.Fatal(err)
	}
	if err := Validate(&pb.TelephonyUpdate{}); err != nil {
		t.Fatal("legacy empty batch rejected")
	}
	for _, change := range []func(*pb.TelephonyHealth){func(h *pb.TelephonyHealth) { h.State = "healthy" }, func(h *pb.TelephonyHealth) {
		h.ResponseCode = nil
		h.LatencyMs = nil
		h.Reachable = false
		h.State = "down"
	}} {
		copy := proto.Clone(base).(*pb.TelephonyUpdate)
		change(copy.Targets[0])
		if err := Validate(copy); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []func(*pb.TelephonyUpdate){
		func(u *pb.TelephonyUpdate) {
			u.Targets = append(u.Targets, proto.Clone(u.Targets[0]).(*pb.TelephonyHealth))
		},
		func(u *pb.TelephonyUpdate) { u.Targets[0].Reachable = false },
		func(u *pb.TelephonyUpdate) { n := math.NaN(); u.Targets[0].LatencyMs = &n },
		func(u *pb.TelephonyUpdate) { n := -0.1; u.Targets[0].LatencyMs = &n },
		func(u *pb.TelephonyUpdate) { u.Targets[0].Transport = "http" },
		func(u *pb.TelephonyUpdate) { u.Targets[0].TargetId = strings.Repeat("x", 1025) },
		func(u *pb.TelephonyUpdate) { u.Targets[0].ObservedAt = &timestamppb.Timestamp{Seconds: math.MaxInt64} },
		func(u *pb.TelephonyUpdate) { u.Targets = make([]*pb.TelephonyHealth, 257) },
	} {
		copy := proto.Clone(base).(*pb.TelephonyUpdate)
		change(copy)
		if Validate(copy) == nil {
			t.Fatal("invalid SIP update accepted")
		}
	}
}
func TestHealthReportsFreshnessAndSourceConnection(t *testing.T) {
	now := time.Now().UTC()
	for _, c := range []struct {
		state     string
		reachable bool
		status    string
	}{{"up", true, "up"}, {"healthy", true, "up"}, {"degraded", true, "degraded"}, {"degraded", false, "degraded"}, {"down", false, "down"}, {"unknown", false, "unknown"}, {"up", false, "unknown"}, {"down", true, "unknown"}} {
		t.Run(c.state+"/"+c.status, func(t *testing.T) {
			s := &Sample{State: c.state, Reachable: c.reachable, ObservedAt: now, ReceivedAt: now}
			h := Evaluate(now, s, nil, true, 30)
			if h.Status != c.status || h.ReportedState == nil || *h.ReportedState != c.state {
				t.Fatalf("wrong report %+v", h)
			}
		})
	}
	s := &Sample{State: "up", Reachable: true, ObservedAt: now, ReceivedAt: now}
	if h := Evaluate(now, s, nil, false, 30); h.Status != "stale" || h.Reason != "agent_disconnected" || !*h.ReportedReachable {
		t.Fatal("disconnect invented a SIP failure")
	}
	if h := Evaluate(now.Add(66*time.Second), s, nil, true, 30); h.Status != "stale" {
		t.Fatal("observation never expires")
	}
	if h := Evaluate(now.Add(10*time.Hour), s, nil, true, 86400); h.Status != "up" {
		t.Fatal("daily interval considered expired")
	}
	if h := Evaluate(now.Add(48*time.Hour+31*time.Second), s, nil, true, 86400); h.Status != "stale" {
		t.Fatal("daily interval never expires")
	}
	if h := Evaluate(now, nil, nil, false, 30); h.Status != "unknown" || h.ReportedReachable != nil {
		t.Fatal("missing data invented false reachability")
	}
}
func TestClockIssueDoesNotReplaceQualifiedStateAndCanClear(t *testing.T) {
	now := time.Now().UTC()
	good := &Sample{State: "up", Reachable: true, ObservedAt: now, ReceivedAt: now}
	issue := &Sample{ClockSkew: true, ObservedAt: now.Add(24 * time.Hour), ReceivedAt: now.Add(time.Second)}
	if h := Evaluate(now.Add(time.Second), good, issue, true, 30); h.Status != "up" || !h.ClockSkew {
		t.Fatal("clock issue replaced last good report")
	}
	if h := Evaluate(now, nil, issue, true, 30); h.Status != "unknown" || h.Reason != "clock_skew" || h.ReportedState != nil {
		t.Fatal("future-only report treated as qualified")
	}
	good.ReceivedAt = now.Add(2 * time.Second)
	if h := Evaluate(now, good, issue, true, 30); h.ClockSkew {
		t.Fatal("new qualified report did not clear clock diagnostic")
	}
	if h := Evaluate(now.Add(-time.Hour), good, nil, true, 30); h.Status != "unknown" || !h.ClockSkew || h.Reason != "clock_skew" {
		t.Fatal("server clock regression advertised good health")
	}
}
func TestIntervalSettingsRemainManualAndBounded(t *testing.T) {
	for _, i := range []int{3, 30, 86400} {
		if err := ValidateInterval(i); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []int{0, 2, 86401} {
		if ValidateInterval(i) == nil {
			t.Fatal("bad interval accepted")
		}
	}
	m := DescribeInterval("simulated:sip-001", 86400, "target_override")
	if m.StaleAfterSeconds != 172830 || m.ApplicationStatus != "unverified" || m.DeliveryMode != "manual_agent_yaml" || !strings.Contains(m.AgentYAMLFragment, `"simulated:sip-001"`) {
		t.Fatalf("unsafe or misleading settings %+v", m)
	}
}
