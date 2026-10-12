package logbook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const MaxRecords = 1024

type Prepared struct {
	Record            Record
	Scope             string
	Fingerprint       []byte
	LegacyFingerprint []byte
	SearchText        string
}

func Validate(batch *pb.LogBatch) error {
	if batch == nil || len(batch.Records) > MaxRecords {
		return errors.New("invalid log batch or record limit")
	}
	if batch.ObservedAt != nil && batch.ObservedAt.CheckValid() != nil {
		return errors.New("invalid log batch timestamp")
	}
	for _, r := range batch.Records {
		if r == nil || len(r.Id) > 256 || len(r.DeviceId) > 256 || len(r.Source) > 256 || len(r.SourceId) > 256 {
			return errors.New("invalid log record identity")
		}
		if r.ObservedAt != nil && r.ObservedAt.CheckValid() != nil || r.OriginalTimestamp != nil && r.OriginalTimestamp.CheckValid() != nil {
			return errors.New("invalid log record timestamp")
		}
		if len(r.Message) > 256<<10 || len(r.Raw) > 256<<10 || len(r.FileIdentity) > 1024 {
			return errors.New("log text exceeds projection limits")
		}
		if r.Audit != nil && (len(r.Audit.EventId) > 256 || len(r.Audit.RecordType) > 128 || len(r.Audit.Executable) > 4096) {
			return errors.New("audit fields exceed limits")
		}
	}
	return nil
}

func Prepare(e *pb.AgentEnvelope, localID string, received time.Time) ([]Prepared, error) {
	batch := e.GetLogs()
	if err := Validate(batch); err != nil {
		return nil, err
	}
	items := make([]Prepared, 0, len(batch.Records))
	batchAt := e.SentAt.AsTime().UTC()
	if batch.ObservedAt != nil {
		batchAt = batch.ObservedAt.AsTime().UTC()
	}
	for i, r := range batch.Records {
		d := Record{AgentID: e.AgentId, DeviceID: r.DeviceId, RecordID: r.Id, MessageID: e.MessageId, Source: r.Source, SourceID: r.SourceId, ObservedAt: batchAt, ReceivedAt: received.UTC(), Severity: r.Severity, Facility: r.Facility,
			Host: r.Host, Process: r.Process, ProcessID: r.ProcessId, Message: r.Message, Raw: r.Raw, Truncated: r.Truncated, FileIdentity: r.FileIdentity, Offset: r.Offset}
		if d.DeviceID == "" {
			d.DeviceID = localID
		}
		if d.RecordID == "" {
			d.RecordID = fmt.Sprintf("message:%s:%d", e.MessageId, i)
		}
		if d.Source == "" {
			d.Source = batch.Source
		}
		if d.Source == "" {
			d.Source = "unknown"
		}
		if r.ObservedAt != nil {
			d.ObservedAt = r.ObservedAt.AsTime().UTC()
		}
		d.EventAt, d.TimeBasis = d.ObservedAt, "observed_at"
		if r.OriginalTimestamp != nil {
			v := r.OriginalTimestamp.AsTime().UTC()
			d.OriginalTimestamp = &v
			d.EventAt = v
			d.TimeBasis = "original_timestamp"
		}
		d.SeverityName = SeverityName(d.Severity)
		scope := ""
		if d.Source == "simulated" || batch.Source == "simulated" || r.GetObservation().GetSource().GetProvenance() == "simulated" || batch.GetObservation().GetSource().GetProvenance() == "simulated" {
			scope = e.SessionId
		}
		// File cursor identity is stable across rereads; collection time is not.
		business := proto.Clone(r).(*pb.LogRecord)
		business.Id, business.DeviceId, business.Source = d.RecordID, d.DeviceID, d.Source
		business.ObservedAt = nil
		// Collection time can change when a stable file record is reread.
		// Source time, source identity and syslog transport evidence remain.
		if business.Observation != nil && business.Observation.Time != nil {
			business.Observation.Time.ObservedAt = nil
			business.Observation.Time.CollectedAt = nil
			business.Observation.Time.ClockOffset = nil
			business.Observation.Time.Uncertainty = nil
			business.Observation.Time.ClockQuality = ""
		}
		encoded, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(business)
		if err != nil {
			return nil, err
		}
		fingerprint := sha256.Sum256(encoded)
		var legacyFingerprint []byte
		if business.Syslog == nil && business.Observation != nil {
			legacy := proto.Clone(business).(*pb.LogRecord)
			legacy.Observation = nil
			oldJSON, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(legacy)
			if err != nil {
				return nil, err
			}
			oldHash := sha256.Sum256(oldJSON)
			legacyFingerprint = oldHash[:]
		}
		if a := r.Audit; a != nil {
			auditHost := a.Host
			if auditHost == "" {
				auditHost = d.Host
			}
			eventIdentity := []string{e.AgentId, d.DeviceID, scope, d.Source, d.SourceID, d.FileIdentity, auditHost, a.EventId}
			if a.EventId == "" {
				eventIdentity = append(eventIdentity, d.RecordID)
			}
			keyBytes, _ := json.Marshal(eventIdentity)
			keyHash := sha256.Sum256(keyBytes)
			d.Audit = &Audit{EventKey: "audit:" + hex.EncodeToString(keyHash[:]), EventID: a.EventId, RecordType: a.RecordType, UID: a.Uid, AUID: a.Auid, PID: a.Pid, Executable: a.Executable, Syscall: a.Syscall, Success: a.Success, Result: a.Result, Host: auditHost}
		}
		sanitizeRecord(&d)
		if r.Syslog != nil {
			public := proto.Clone(r.Syslog).(*pb.SyslogMetadata)
			d.RawPayloadOmitted = len(public.RawPayload) > 0
			public.RawPayload = nil // Binary octets remain in the inbox, not searchable REST.
			for _, v := range []*string{&public.StructuredData, &public.MessageId, &public.RawTimestamp, &public.PeerIp, &public.Transport, &public.TlsIdentity} {
				before := *v
				*v = scrub(before)
				d.Redacted = d.Redacted || before != *v
			}
			d.Syslog, err = (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(public)
			if err != nil {
				return nil, err
			}
		}
		metadata := r.Observation
		if metadata == nil {
			metadata = batch.Observation
		}
		if metadata != nil {
			d.Observation, err = (protojson.MarshalOptions{UseProtoNames: true}).Marshal(metadata)
			if err != nil {
				return nil, err
			}
		}
		text := []string{d.Host, d.Process, d.ProcessID, d.Message, d.Raw}
		if d.Audit != nil {
			text = append(text, d.Audit.Executable, d.Audit.Syscall, d.Audit.Result, d.Audit.Host)
		}
		items = append(items, Prepared{Record: d, Scope: scope, Fingerprint: fingerprint[:], LegacyFingerprint: legacyFingerprint, SearchText: strings.Join(text, "\n")})
	}
	return items, nil
}
