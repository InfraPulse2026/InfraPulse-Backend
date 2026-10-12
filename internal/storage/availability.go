package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/availability"
	"github.com/jackc/pgx/v5"
)

var ErrEventConflict = errors.New("availability event ID reused with different contents")
var ErrInvalidAvailability = errors.New("invalid availability evidence")

func recognizedAvailabilityEvent(v *pb.Event) bool {
	return v != nil && (v.Type == "device.online" || v.Type == "device.offline") && (v.Source == "remote" || v.Source == "simulated")
}

func validateAvailabilityEvents(batch *pb.EventBatch) error {
	if batch == nil {
		return nil
	}
	for _, v := range batch.Events {
		if !recognizedAvailabilityEvent(v) {
			continue
		}
		if v.Id == "" || len(v.Id) > 256 || v.DeviceId == "" || len(v.DeviceId) > 256 || v.Timestamp == nil || v.Timestamp.CheckValid() != nil {
			return fmt.Errorf("%w: device events require bounded event/device IDs and a valid timestamp", ErrInvalidAvailability)
		}
	}
	return nil
}

func projectAvailability(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	switch e.Payload.(type) {
	case *pb.AgentEnvelope_Hello, *pb.AgentEnvelope_Heartbeat, *pb.AgentEnvelope_Inventory, *pb.AgentEnvelope_Telemetry, *pb.AgentEnvelope_Events:
	default:
		return nil
	}
	var received time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&received); err != nil {
		return err
	}
	received = received.UTC()
	switch p := e.Payload.(type) {
	case *pb.AgentEnvelope_Hello:
		return inventoryAvailability(ctx, tx, e, p.Hello.Device, at(p.Hello.Device.LastSeen, e.SentAt.AsTime()), "hello", received)
	case *pb.AgentEnvelope_Heartbeat:
		var localID string
		if err := tx.QueryRow(ctx, `SELECT local_device_id FROM agents WHERE agent_id=$1`, e.AgentId).Scan(&localID); err != nil {
			return err
		}
		return recordAvailability(ctx, tx, e.AgentId, localID, availability.Observation{Kind: "heartbeat", Source: "agent", MessageID: e.MessageId, Online: true, ObservedAt: received, ReceivedAt: received, TimeBasis: "server_receipt"})
	case *pb.AgentEnvelope_Inventory:
		for _, d := range p.Inventory.Devices {
			if err := inventoryAvailability(ctx, tx, e, d, p.Inventory.ObservedAt.AsTime(), "inventory", received); err != nil {
				return err
			}
		}
	case *pb.AgentEnvelope_Telemetry:
		for _, sample := range p.Telemetry.Samples {
			if err := recordAvailability(ctx, tx, e.AgentId, sample.DeviceId, availability.Observation{Kind: "telemetry", Source: sample.Source, MessageID: e.MessageId, Online: true, ObservedAt: sample.ObservedAt.AsTime(), ReceivedAt: received, TimeBasis: "agent_observed"}); err != nil {
				return err
			}
		}
	case *pb.AgentEnvelope_Events:
		if err := validateAvailabilityEvents(p.Events); err != nil {
			return err
		}
		for _, v := range p.Events.Events {
			if !recognizedAvailabilityEvent(v) {
				continue
			}
			original, err := wireJSON(v)
			if err != nil {
				return err
			}
			// The development Simulator intentionally reuses deterministic IDs
			// in separate runs. Real Agent event UUIDs remain globally scoped.
			scope := ""
			if v.Source == "simulated" {
				scope = e.SessionId
			}
			result, err := tx.Exec(ctx, `INSERT INTO availability_event_ids(agent_id,event_scope,event_id,payload)
			VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, e.AgentId, scope, v.Id, original)
			if err != nil {
				return err
			}
			if result.RowsAffected() == 0 {
				var identical bool
				if err = tx.QueryRow(ctx, `SELECT payload=$4::jsonb FROM availability_event_ids WHERE agent_id=$1 AND event_scope=$2 AND event_id=$3`, e.AgentId, scope, v.Id, original).Scan(&identical); err != nil {
					return err
				}
				if !identical {
					return ErrEventConflict
				}
				continue
			}
			kind := "event_offline"
			if v.Type == "device.online" {
				kind = "event_online"
			}
			if err = recordAvailability(ctx, tx, e.AgentId, v.DeviceId, availability.Observation{Kind: kind, Source: v.Source, MessageID: e.MessageId, EventID: v.Id, Online: v.Type == "device.online", ObservedAt: v.Timestamp.AsTime(), ReceivedAt: received, TimeBasis: "agent_observed"}); err != nil {
				return err
			}
		}
	}
	return nil
}

func inventoryAvailability(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope, d *pb.Device, observed time.Time, kind string, received time.Time) error {
	// False without last_seen is indistinguishable from a protobuf default.
	if !d.Online && d.LastSeen == nil {
		return nil
	}
	if d.Source == "" {
		return nil
	}
	if d.Online && d.LastSeen != nil && d.LastSeen.AsTime().Before(observed) {
		observed = d.LastSeen.AsTime()
	}
	return recordAvailability(ctx, tx, e.AgentId, d.Id, availability.Observation{Kind: kind, Source: d.Source, MessageID: e.MessageId, Online: d.Online, ObservedAt: observed.UTC(), ReceivedAt: received, TimeBasis: "agent_observed"})
}

func recordAvailability(ctx context.Context, tx pgx.Tx, agentID, deviceID string, o availability.Observation) error {
	if deviceID == "" {
		return ErrInvalidAvailability
	}
	_, err := tx.Exec(ctx, `INSERT INTO devices(agent_id,device_id,name,source,state_at,last_seen)
	VALUES($1,$2,$2,$3,$4,$4) ON CONFLICT(agent_id,device_id) DO NOTHING`, agentID, deviceID, o.Source, o.ObservedAt)
	if err != nil {
		return err
	}
	// The device row is the common lock for all producers of its projection.
	if _, err = tx.Exec(ctx, `SELECT 1 FROM devices WHERE agent_id=$1 AND device_id=$2 FOR UPDATE`, agentID, deviceID); err != nil {
		return err
	}
	skew := o.ObservedAt.After(o.ReceivedAt.Add(30 * time.Second))
	priority := availability.Priority(o.Kind, o.Online)
	_, err = tx.Exec(ctx, `INSERT INTO availability_sources(agent_id,device_id,kind,clock_skew,online,observed_at,received_at,source,message_id,event_id,time_basis,priority)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	ON CONFLICT(agent_id,device_id,kind,clock_skew) DO UPDATE SET
	online=EXCLUDED.online,observed_at=EXCLUDED.observed_at,received_at=EXCLUDED.received_at,source=EXCLUDED.source,
	message_id=EXCLUDED.message_id,event_id=EXCLUDED.event_id,time_basis=EXCLUDED.time_basis,priority=EXCLUDED.priority
	WHERE
	(availability_sources.clock_skew AND EXCLUDED.received_at>=availability_sources.received_at)
	OR (NOT availability_sources.clock_skew AND
	 (EXCLUDED.observed_at>availability_sources.observed_at OR
	  (EXCLUDED.observed_at=availability_sources.observed_at AND
	   (EXCLUDED.priority>availability_sources.priority OR EXCLUDED.priority=availability_sources.priority AND EXCLUDED.received_at>=availability_sources.received_at))))`,
		agentID, deviceID, o.Kind, skew, o.Online, o.ObservedAt, o.ReceivedAt, o.Source, o.MessageID, o.EventID, o.TimeBasis, priority)
	if err != nil || skew {
		return err
	}
	var before *bool
	var atBefore time.Time
	var priorityBefore int
	var receivedBefore time.Time
	err = tx.QueryRow(ctx, `SELECT online,observed_at,priority,received_at FROM device_availability_current WHERE agent_id=$1 AND device_id=$2`, agentID, deviceID).Scan(&before, &atBefore, &priorityBefore, &receivedBefore)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if before != nil && (o.ObservedAt.Before(atBefore) || o.ObservedAt.Equal(atBefore) &&
		(priority < priorityBefore || priority == priorityBefore && o.ReceivedAt.Before(receivedBefore))) {
		return nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO device_availability_current(agent_id,device_id,online,observed_at,received_at,kind,source,message_id,event_id,time_basis,priority)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	ON CONFLICT(agent_id,device_id) DO UPDATE SET online=EXCLUDED.online,observed_at=EXCLUDED.observed_at,received_at=EXCLUDED.received_at,
	kind=EXCLUDED.kind,source=EXCLUDED.source,message_id=EXCLUDED.message_id,event_id=EXCLUDED.event_id,time_basis=EXCLUDED.time_basis,priority=EXCLUDED.priority`,
		agentID, deviceID, o.Online, o.ObservedAt, o.ReceivedAt, o.Kind, o.Source, o.MessageID, o.EventID, o.TimeBasis, priority)
	if err != nil {
		return err
	}
	if before == nil || *before != o.Online {
		_, err = tx.Exec(ctx, `INSERT INTO device_availability_changes(agent_id,device_id,from_online,to_online,observed_at,received_at,kind,source,message_id,event_id,time_basis)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, agentID, deviceID, before, o.Online, o.ObservedAt, o.ReceivedAt, o.Kind, o.Source, o.MessageID, o.EventID, o.TimeBasis)
	}
	return err
}

func (s *Store) AvailabilityChanges(ctx context.Context, agentID, deviceID string, limit, offset int) ([]availability.Change, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,agent_id,device_id,kind,source,COALESCE(message_id,''),event_id,time_basis,from_online,to_online,observed_at,received_at
	FROM device_availability_changes WHERE agent_id=$1 AND device_id=$2 ORDER BY id DESC LIMIT $3 OFFSET $4`, agentID, deviceID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []availability.Change{}
	for rows.Next() {
		var c availability.Change
		if err = rows.Scan(&c.ID, &c.AgentID, &c.DeviceID, &c.Kind, &c.Source, &c.MessageID, &c.EventID, &c.TimeBasis, &c.FromOnline, &c.ToOnline, &c.ObservedAt, &c.ReceivedAt); err != nil {
			return nil, err
		}
		c.ObservedAt, c.ReceivedAt = c.ObservedAt.UTC(), c.ReceivedAt.UTC()
		items = append(items, c)
	}
	return items, rows.Err()
}
