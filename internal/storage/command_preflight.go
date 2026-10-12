package storage

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

func sameResource(a, b string) bool {
	if a == b {
		return true
	}
	one, e1 := net.ParseMAC(a)
	two, e2 := net.ParseMAC(b)
	return e1 == nil && e2 == nil && one.String() == two.String()
}
func managementIP(value string) net.IP {
	if u, err := url.Parse(value); err == nil && u.Host != "" {
		return net.ParseIP(u.Hostname())
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(value)
}
func (s *Store) commandPreflight(ctx context.Context, tx pgx.Tx, r commands.Request, now time.Time) (session, canonical string, err error) {
	policy := s.commandPolicy
	if !policy.Enabled {
		return "", "", commands.ErrDisabled
	}
	if r.Operation == "disable" && !policy.AllowDisable {
		return "", "", commands.ErrDisabled
	}
	if r.Operation == "rollback" && (!policy.AllowRollback || !policy.AllowDisable) {
		return "", "", commands.ErrDisabled
	}
	for _, id := range policy.ProtectedDevices {
		if id == r.DeviceID {
			return "", "", commands.ErrProtected
		}
	}
	var connected bool
	var inventory, sample []byte
	var invAt, telAt *time.Time
	err = tx.QueryRow(ctx, `SELECT a.session_id,a.connected AND a.last_seen>$3,d.inventory,d.inventory_at,t.payload,t.observed_at
 FROM devices d JOIN agents a USING(agent_id)
 LEFT JOIN LATERAL(SELECT payload,observed_at FROM telemetry WHERE agent_id=d.agent_id AND device_id=d.device_id AND observed_at<=received_at+interval '30 seconds' AND observed_at<=$4 ORDER BY observed_at DESC,id DESC LIMIT 1)t ON true
 WHERE d.agent_id=$1 AND d.device_id=$2`, r.AgentID, r.DeviceID, now.Add(-s.timeout), now.Add(30*time.Second)).Scan(&session, &connected, &inventory, &invAt, &sample, &telAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", commands.ErrPrecondition
		}
		return "", "", err
	}
	if !connected || invAt == nil || telAt == nil || invAt.Before(now.Add(-5*time.Minute)) || invAt.After(now.Add(30*time.Second)) || telAt.Before(now.Add(-30*time.Second)) || telAt.After(now.Add(30*time.Second)) {
		return "", "", commands.ErrPrecondition
	}
	states, e := s.availabilityPage(ctx, tx, r.AgentID, r.DeviceID, 2, 0, now)
	if e != nil {
		return "", "", e
	}
	if states[deviceKey{r.AgentID, r.DeviceID}].Status != "online" {
		return "", "", commands.ErrPrecondition
	}
	d := &pb.Device{}
	t := &pb.TelemetrySample{}
	if protojson.Unmarshal(inventory, d) != nil || protojson.Unmarshal(sample, t) != nil {
		return "", "", commands.ErrPrecondition
	}
	if d.Driver != "routeros-rest" || !d.Online || d.LastSeen == nil || d.LastSeen.AsTime().Before(now.Add(-5*time.Minute)) || d.LastSeen.AsTime().After(now.Add(30*time.Second)) || t.Source != "routeros-rest" {
		return "", "", commands.ErrPrecondition
	}
	supported := false
	required := "interfaces." + r.Operation
	if r.Operation == "rollback" {
		required = "resource.rollback"
	}
	for _, cap := range d.Capabilities {
		if cap.Name == required && cap.Supported {
			supported = true
		}
	}
	if !supported {
		return "", "", commands.ErrPrecondition
	}
	var iface *pb.Interface
	for _, candidate := range t.Interfaces {
		if sameResource(r.InterfaceID, candidate.Id) || sameResource(r.InterfaceID, candidate.Name) || candidate.Alias != "" && sameResource(r.InterfaceID, candidate.Alias) || candidate.Mac != "" && sameResource(r.InterfaceID, candidate.Mac) {
			if iface != nil {
				return "", "", commands.ErrPrecondition
			}
			iface = candidate
		}
	}
	if iface == nil || iface.Metadata == nil || !iface.Metadata.AdminStatus || iface.AdminUp != *r.ExpectedAdminUp || iface.Id == "" || len(iface.Id) > 128 {
		return "", "", commands.ErrPrecondition
	}
	for _, protected := range policy.ProtectedInterfaces {
		for _, alias := range []string{r.InterfaceID, iface.Id, iface.Name, iface.Alias, iface.Mac} {
			if sameResource(protected, alias) {
				return "", "", commands.ErrProtected
			}
		}
	}
	mgmt := managementIP(d.ManagementAddress)
	if mgmt != nil {
		for _, address := range iface.Addresses {
			ip := net.ParseIP(address)
			if strings.Contains(address, "/") {
				ip, _, _ = net.ParseCIDR(address)
			}
			if ip != nil && ip.Equal(mgmt) {
				return "", "", commands.ErrProtected
			}
		}
	}
	return session, iface.Id, nil
}
