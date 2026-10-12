package collectorconfig

import (
	"encoding/hex"
	"encoding/json"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"os"
	"testing"
	"time"
)

func TestIndependentAgentGoldenDigest(t *testing.T) {
	var f struct {
		Canonical string `json:"canonical_hex"`
		SHA       string `json:"sha256"`
	}
	b, err := os.ReadFile("testdata/digest-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	s := &pb.DesiredConfigSnapshot{CollectorId: "9801c72e-6308-40db-a5d8-584b5f636a5f", Revision: 1, Targets: []*pb.ManagedTargetSpec{{TargetId: "switch-01", Protocol: "snmp", Enabled: true, Endpoint: "192.0.2.10", CredentialRef: "read"}}}
	bytes, err := CanonicalBytes(s)
	if err != nil || hex.EncodeToString(bytes) != f.Canonical {
		t.Fatal("canonical contract mismatch", err)
	}
	digest, err := Digest(s)
	if err != nil || digest != f.SHA {
		t.Fatal("digest contract mismatch", digest, err)
	}
	s.SessionId = "new-session"
	s.Revision = 120
	s.ConfigurationDigest = "ignored"
	d, _ := Digest(s)
	if d != digest {
		t.Fatal("transport metadata entered digest")
	}
	n, err := Normalize(s)
	if err != nil || Validate(n) != nil {
		t.Fatal("normalized golden rejected", err)
	}
	if s.Targets[0].Polling != nil {
		t.Fatal("caller mutated")
	}
}
func TestProtocolDefaultsBoundsAndClosedConfiguration(t *testing.T) {
	for _, protocol := range []string{"snmp", "routeros", "sip", "redfish", "ipmi"} {
		t.Run(protocol, func(t *testing.T) {
			endpoint := "192.0.2.10"
			credential := "read"
			if protocol == "sip" {
				credential = ""
			}
			if protocol == "redfish" || protocol == "routeros" {
				endpoint = "https://192.0.2.10"
			}
			s := &pb.DesiredConfigSnapshot{CollectorId: "collector", Targets: []*pb.ManagedTargetSpec{{TargetId: "target", Protocol: protocol, Endpoint: endpoint, CredentialRef: credential, Enabled: true}}}
			n, err := Normalize(s)
			if err != nil || Validate(n) != nil {
				t.Fatal("defaults", n, err)
			}
			if protocol == "redfish" || protocol == "ipmi" {
				n.Targets[0].Polling.MediumInterval = durationpb.New(24 * time.Hour)
			} else {
				n.Targets[0].Polling.FastInterval = durationpb.New(24 * time.Hour)
			}
			if Validate(n) != nil {
				t.Fatal("24h rejected")
			}
			if protocol == "redfish" || protocol == "ipmi" {
				n.Targets[0].Polling.MediumInterval = durationpb.New(9 * time.Second)
			} else {
				n.Targets[0].Polling.FastInterval = durationpb.New(24*time.Hour + time.Nanosecond)
			}
			if Validate(n) == nil {
				t.Fatal("out-of-bounds accepted")
			}
			n, _ = Normalize(s)
			n.Targets = append(n.Targets, proto.Clone(n.Targets[0]).(*pb.ManagedTargetSpec))
			if Validate(n) == nil {
				t.Fatal("duplicate ID accepted")
			}
			s.Targets[0].ProtoReflect().SetUnknown([]byte{0xA0, 0x06, 0x01})
			if _, err = Normalize(s); err == nil {
				t.Fatal("unknown field accepted")
			}
		})
	}
}

func TestPublishedExamplesMatchManagedContract(t *testing.T) {
	raw, err := os.ReadFile("../../docs/examples/managed-collector-targets.json")
	if err != nil {
		t.Fatal(err)
	}
	var input Input
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]json.RawMessage{"targets": input.Targets})
	if err != nil {
		t.Fatal(err)
	}
	s := &pb.DesiredConfigSnapshot{}
	if err = protojson.Unmarshal(payload, s); err != nil {
		t.Fatal(err)
	}
	s.CollectorId = "example-collector"
	s, err = Normalize(s)
	if err != nil || Validate(s) != nil || len(s.Targets) != 5 {
		t.Fatal("unusable documentation example", s, err)
	}
}
