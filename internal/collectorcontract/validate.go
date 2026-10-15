// Package collectorcontract validates additive Collector metadata without
// changing legacy payload identity or applying source clock corrections.
package collectorcontract

import (
	"encoding/base64"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
)

func text(s string, limit int) bool { return len(s) <= limit && utf8.ValidString(s) }
func identifier(s string, limit int) bool {
	if !text(s, limit) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func member(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func source(s *pb.SourceRef, agent string) error {
	if s == nil || !member(s.Type, "local", "snmp", "routeros", "lldp", "cdp", "mndp", "sip", "syslog", "auditd", "redfish", "ipmi") {
		return errors.New("invalid observation source type")
	}
	if !identifier(s.CollectorId, 256) || !identifier(s.TargetId, 256) || !identifier(s.InstanceId, 256) || !identifier(s.Protocol, 128) {
		return errors.New("invalid observation source identity")
	}
	if s.CollectorId != "" && s.CollectorId != agent {
		return errors.New("observation collector identity differs from stream")
	}
	if !member(s.Provenance, "", "real", "simulated", "manual", "external") {
		return errors.New("invalid observation provenance")
	}
	if s.ResourceId != "" {
		u, err := url.Parse(s.ResourceId)
		if err != nil || !identifier(s.ResourceId, 2048) || !strings.HasPrefix(s.ResourceId, "/") || strings.HasPrefix(s.ResourceId, "//") || u.IsAbs() || u.Host != "" || u.User != nil {
			return errors.New("invalid resource identifier")
		}
	}
	return nil
}
func duration(d *durationpb.Duration, nonnegative bool) error {
	if d == nil {
		return nil
	}
	if d.CheckValid() != nil {
		return errors.New("invalid observation duration")
	}
	roundtrip := durationpb.New(d.AsDuration())
	if roundtrip.Seconds != d.Seconds || roundtrip.Nanos != d.Nanos || nonnegative && d.AsDuration() < 0 {
		return errors.New("out of range observation duration")
	}
	return nil
}
func observation(o *pb.ObservationMetadata, agent string) error {
	if err := source(o.Source, agent); err != nil {
		return err
	}
	return observationTime(o.Time)
}
func observationTime(at *pb.ObservationTime) error {
	if at == nil || at.ObservedAt == nil || at.CollectedAt == nil || at.ObservedAt.CheckValid() != nil || at.CollectedAt.CheckValid() != nil || at.SourceTimestamp != nil && at.SourceTimestamp.CheckValid() != nil {
		return errors.New("invalid observation time")
	}
	if !member(at.ClockQuality, "", "unknown", "synced", "acceptable", "degraded", "unsynced") {
		return errors.New("invalid clock quality")
	}
	if err := duration(at.ClockOffset, false); err != nil {
		return err
	}
	return duration(at.Uncertainty, true)
}
func status(s *pb.HardwareStatus) error {
	if s == nil {
		return nil
	}
	if !member(s.Health, "", "ok", "warning", "critical", "unknown") || !member(s.HealthRollup, "", "ok", "warning", "critical", "unknown") || !identifier(s.State, 128) {
		return errors.New("invalid hardware status")
	}
	return nil
}
func hardware(h *pb.HardwareInventory) error {
	if h.ResourceType == "" || len(h.Relations) > 1024 || len(h.NetworkAddresses) > 256 {
		return errors.New("invalid hardware inventory bounds")
	}
	for _, s := range []string{h.ResourceType, h.HardwareUuid, h.Sku, h.PartNumber, h.BiosVersion, h.PowerState, h.ChassisType, h.ManagerType, h.AssetTag, h.PhysicalSecurity, h.DeviceLocation, h.MemoryType, h.ErrorCorrection, h.Protocol, h.MediaType} {
		if !identifier(s, 1024) {
			return errors.New("invalid hardware text")
		}
	}
	for _, s := range h.NetworkAddresses {
		if !identifier(s, 1024) {
			return errors.New("invalid hardware address")
		}
	}
	for _, r := range h.Relations {
		if r == nil || r.Type == "" || !identifier(r.Type, 128) || r.TargetResourceId == "" || !identifier(r.TargetResourceId, 2048) {
			return errors.New("invalid hardware relationship")
		}
		if err := source(&pb.SourceRef{Type: "redfish", ResourceId: r.TargetResourceId}, ""); err != nil {
			return err
		}
	}
	return status(h.Status)
}
func identity(e *pb.IdentityEvidence, agent string) error {
	if !member(e.Type, "serial", "hardware_uuid", "mac", "chassis_id", "lldp_chassis_id", "management_ip", "hostname", "sys_name", "sys_object_id", "manufacturer", "model", "ip", "interface_name") || e.Value == "" || !identifier(e.Value, 4096) || !identifier(e.SubjectId, 256) || len(e.RawValue) > 1024 || !text(e.OriginalValue, 4096) || !text(e.Origin, 256) || !member(e.SubjectKind, "", "network_device", "network_interface", "computer_system", "chassis", "manager", "component", "remote_neighbor") || !member(e.Normalization, "", "canonical", "invalid", "rejected") {
		return errors.New("invalid identity evidence")
	}
	if len(e.RawValue) > 0 && e.Value != base64.StdEncoding.EncodeToString(e.RawValue) {
		return errors.New("binary identity mismatch")
	}
	if e.Subtype != nil && (len(e.RawValue) == 0 || *e.Subtype > 7 || !member(e.Type, "chassis_id", "lldp_chassis_id")) {
		return errors.New("invalid chassis subtype")
	}
	if !member(e.Confidence, "high", "medium", "low", "unresolved") {
		return errors.New("invalid identity confidence")
	}
	return source(e.Source, agent)
}

func configStatus(v *pb.ConfigApplyStatus, agent string) error {
	validDigest := func(value string) bool {
		if len(value) != 64 {
			return false
		}
		for _, c := range value {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
	if v.CollectorId != agent || v.Revision == 0 || !validDigest(v.ConfigurationDigest) || v.Status < pb.ConfigStatus_CONFIG_STATUS_RECEIVED || v.Status > pb.ConfigStatus_CONFIG_STATUS_FAILED || !identifier(v.ErrorCode, 128) || !text(v.SanitizedErrorMessage, 4096) || (v.AppliedRevision == 0 && v.AppliedDigest != "") || (v.AppliedRevision > 0 && !validDigest(v.AppliedDigest)) {
		return errors.New("invalid managed configuration status")
	}
	return nil
}
func syslog(s *pb.SyslogMetadata) error {
	if s.Version > 999 || !member(s.Format, "rfc5424", "rfc3164", "unknown") || !member(s.ParseStatus, "parsed", "fallback") || !member(s.Transport, "", "tcp", "tls") {
		return errors.New("invalid syslog metadata")
	}
	if !text(s.StructuredData, 65536) || !text(s.MessageId, 1024) || !text(s.RawTimestamp, 1024) || !text(s.TlsIdentity, 1024) || !text(s.PeerIp, 128) || len(s.RawPayload) > 65536 || len(s.RawPayload) > 0 && utf8.Valid(s.RawPayload) {
		return errors.New("invalid syslog bounds")
	}
	if s.PeerIp != "" {
		ip, err := netip.ParseAddr(s.PeerIp)
		if err != nil || ip.Zone() != "" {
			return errors.New("invalid syslog peer")
		}
	}
	if s.Authenticated && (s.Transport != "tls" || s.TlsIdentity == "") || s.TlsIdentity != "" && (!s.Authenticated || s.Transport != "tls") {
		return errors.New("invalid syslog TLS attribution")
	}
	return nil
}

// Validate is called both at the stream boundary and before storage. Nil
// additive metadata remains valid for legacy Collectors.
func Validate(e *pb.AgentEnvelope) error {
	if e == nil {
		return errors.New("missing envelope")
	}
	return walk(e.ProtoReflect(), e.AgentId)
}
func walk(m protoreflect.Message, agent string) error {
	if !m.IsValid() {
		return errors.New("missing message")
	}
	var err error
	switch v := m.Interface().(type) {
	case *pb.InventoryUpdate:
		if _, ok := pb.SnapshotCompleteness_name[int32(v.Completeness)]; !ok {
			err = errors.New("invalid inventory completeness")
		}
	case *pb.ObservationMetadata:
		err = observation(v, agent)
	case *pb.ObservationTime:
		err = observationTime(v)
	case *pb.ConfigApplyStatus:
		err = configStatus(v, agent)
	case *pb.AppliedConfigState:
		if !identifier(v.ConfigurationDigest, 64) || v.Status < pb.ConfigStatus_CONFIG_STATUS_UNSPECIFIED || v.Status > pb.ConfigStatus_CONFIG_STATUS_FAILED || v.TargetCount > 4096 {
			err = errors.New("invalid applied configuration state")
		}
	case *pb.ManagementAddress:
		if !identifier(v.Address, 128) || len(v.RawAddress) > 256 {
			err = errors.New("invalid advertised management address")
		}
	case *pb.HardwareInventory:
		err = hardware(v)
	case *pb.HardwareStatus:
		err = status(v)
	case *pb.IdentityEvidence:
		err = identity(v, agent)
	case *pb.SyslogMetadata:
		err = syslog(v)
	case *pb.ComponentHealth:
		if v.Id == "" || !identifier(v.Id, 256) || !identifier(v.ResourceId, 2048) || !text(v.Name, 1024) {
			err = errors.New("invalid hardware component")
		}
	}
	if err != nil {
		return err
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		if fd.IsList() {
			if v.List().Len() > 4096 {
				err = errors.New("metadata list exceeds limit")
				return false
			}
			for i := 0; i < v.List().Len(); i++ {
				if err = walk(v.List().Get(i).Message(), agent); err != nil {
					return false
				}
			}
		} else {
			err = walk(v.Message(), agent)
		}
		return err == nil
	})
	return err
}
