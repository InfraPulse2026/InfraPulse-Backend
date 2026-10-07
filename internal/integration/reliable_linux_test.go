//go:build linux

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/api"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/controller"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/monitoring"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These tests use a separate, unmodified Linux Agent executable, actual TCP
// gRPC/HTTP, a separate Backend process, and an isolated PostgreSQL schema. All
// fault injection lives in the test stream wrapper, never in production code.
// Crosscompile with: GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c ./internal/integration
// Run with AGENT_BINARY and TEST_DATABASE_URL; E2E_ARTIFACTS optionally retains
// JSONL wire attempts, post-commit ACK evidence, diagnostics and process logs.
type wireEvent struct {
	Type       string          `json:"type"`
	At         time.Time       `json:"at"`
	AgentID    string          `json:"agent_id,omitempty"`
	SessionID  string          `json:"session_id,omitempty"`
	MessageID  string          `json:"message_id,omitempty"`
	Sequence   uint64          `json:"sequence,omitempty"`
	SentAt     time.Time       `json:"sent_at,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Committed  int             `json:"committed_rows,omitempty"`
	DomainRows int             `json:"domain_rows,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

type ackFault struct {
	Mode string `json:"mode"`
	Kind string `json:"kind"`
}

type faultController struct {
	pb.UnimplementedAgentServiceServer
	controller *controller.Controller
	db         *pgx.Conn
	dir        string
	mu         sync.Mutex
	trace      *os.File
}

func (c *faultController) record(e wireEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.At = time.Now().UTC()
	_ = json.NewEncoder(c.trace).Encode(e)
	_ = c.trace.Sync()
}

func (c *faultController) Connect(s grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
	return c.controller.Connect(&observedStream{BidiStreamingServer: s, controller: c})
}

type observedStream struct {
	grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]
	controller *faultController
	current    *pb.AgentEnvelope
}

func (s *observedStream) Recv() (*pb.AgentEnvelope, error) {
	e, err := s.BidiStreamingServer.Recv()
	if err == nil {
		s.current = e
		b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(e)
		s.controller.record(wireEvent{Type: "receive", AgentID: e.AgentId, SessionID: e.SessionId, MessageID: e.MessageId, Sequence: e.Sequence, SentAt: e.SentAt.AsTime(), Kind: storage.Kind(e), Payload: b})
	}
	return e, err
}

func (s *observedStream) Send(e *pb.ControllerEnvelope) error {
	c := s.controller
	if h := e.GetHelloAck(); h != nil {
		if !h.Accepted || h.ProtocolVersion != 1 || !h.ApplicationAck {
			return status.Error(codes.Internal, "P3 application ACK was not negotiated")
		}
		var count int
		c.mu.Lock()
		err := c.db.QueryRow(s.Context(), `SELECT count(*) FROM agent_messages m JOIN agents a USING(agent_id) JOIN agent_sessions sess ON sess.agent_id=a.agent_id AND sess.session_id=a.session_id JOIN devices d ON d.agent_id=a.agent_id AND d.device_id=a.local_device_id WHERE m.agent_id=$1 AND m.message_id=$2 AND a.session_id=$3`, s.current.AgentId, s.current.MessageId, s.current.SessionId).Scan(&count)
		c.mu.Unlock()
		if err != nil || count != 1 {
			return status.Errorf(codes.Internal, "HelloAck precedes committed registration: count=%d error=%v", count, err)
		}
		c.record(wireEvent{Type: "hello_ack", AgentID: s.current.AgentId, SessionID: s.current.SessionId, MessageID: s.current.MessageId})
	}
	if ack := e.GetMessageAck(); ack != nil {
		// An independent connection must see the inbox row before ACK is sent:
		// uncommitted writes from Ingest's transaction cannot satisfy this check.
		var count int
		c.mu.Lock()
		err := c.db.QueryRow(s.Context(), `SELECT count(*) FROM agent_messages WHERE agent_id=$1 AND message_id=$2`, s.current.AgentId, ack.MessageId).Scan(&count)
		c.mu.Unlock()
		if err != nil || count != 1 {
			return status.Errorf(codes.Internal, "ACK precedes durable commit: count=%d error=%v", count, err)
		}
		ev := wireEvent{Type: "ack", AgentID: s.current.AgentId, SessionID: s.current.SessionId, MessageID: ack.MessageId, Kind: storage.Kind(s.current), Committed: count}
		if telemetry := s.current.GetTelemetry(); telemetry != nil {
			c.mu.Lock()
			err := c.db.QueryRow(s.Context(), `SELECT count(*) FROM telemetry WHERE agent_id=$1 AND message_id=$2`, s.current.AgentId, ack.MessageId).Scan(&ev.DomainRows)
			c.mu.Unlock()
			if err != nil || ev.DomainRows != len(telemetry.Samples) {
				return status.Errorf(codes.Internal, "ACK precedes domain persistence: rows=%d expected=%d error=%v", ev.DomainRows, len(telemetry.Samples), err)
			}
		}
		var fault ackFault
		if b, err := os.ReadFile(filepath.Join(c.dir, "fault.json")); err == nil {
			_ = json.Unmarshal(b, &fault)
		}
		if fault.Kind == ev.Kind && (fault.Mode == "drop" || fault.Mode == "hold") {
			ev.Type = "ack_" + fault.Mode
			c.record(ev)
			b, _ := json.Marshal(ev)
			if err := os.WriteFile(filepath.Join(c.dir, "fault-hit.json"), b, 0600); err != nil {
				return err
			}
			if fault.Mode == "drop" {
				_ = os.Remove(filepath.Join(c.dir, "fault.json"))
				return status.Error(codes.Unavailable, "test deliberately lost ACK after COMMIT")
			}
			<-s.Context().Done()
			return s.Context().Err()
		}
		if err := s.BidiStreamingServer.Send(e); err != nil {
			return err
		}
		c.record(ev)
		return nil
	}
	return s.BidiStreamingServer.Send(e)
}

// TestReliableBackendProcess is the child process entry point. Calling the
// production Controller and Store here preserves real transaction and restart
// behavior while allowing deterministic ACK loss at the gRPC boundary.
func TestReliableBackendProcess(t *testing.T) {
	if os.Getenv("INFRAPULSE_E2E_BACKEND_CHILD") != "1" {
		t.Skip("process helper")
	}
	ctx := context.Background()
	dbURL := os.Getenv("INFRAPULSE_E2E_DB")
	s, err := storage.Open(ctx, dbURL, 90*time.Second, monitoring.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	dir := os.Getenv("INFRAPULSE_E2E_DIR")
	trace, err := os.OpenFile(filepath.Join(dir, "wire.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := &faultController{controller: controller.New(s, log), db: db, dir: dir, trace: trace}
	lis, err := net.Listen("tcp", os.Getenv("INFRAPULSE_E2E_GRPC"))
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	pb.RegisterAgentServiceServer(g, f)
	go func() { _ = g.Serve(lis) }()
	t.Fatal(http.ListenAndServe(os.Getenv("INFRAPULSE_E2E_HTTP"), api.New(s, log)))
}

type fixture struct {
	t              *testing.T
	dir, dbURL     string
	grpc, http     string
	db             *pgx.Conn
	backend, agent *exec.Cmd
	backendRuns    int
	agentRuns      int
	config         string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" || os.Getenv("AGENT_BINARY") == "" {
		t.Skip("requires TEST_DATABASE_URL and AGENT_BINARY (real Linux Agent)")
	}
	dir := t.TempDir()
	if root := os.Getenv("E2E_ARTIFACTS"); root != "" {
		dir = filepath.Join(root, fmt.Sprintf("%s_%d", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano()))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := pgx.Connect(context.Background(), baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("reliable_e2e_%d", time.Now().UnixNano())
	if _, err = admin.Exec(context.Background(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	dbURL := parsed.String()
	db, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: dir, dbURL: dbURL, grpc: freeAddress(t), http: freeAddress(t), db: db, config: filepath.Join(dir, "agent.yaml")}
	t.Cleanup(func() {
		killProcess(f.agent)
		killProcess(f.backend)
		_ = db.Close(context.Background())
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = admin.Close(context.Background())
	})
	f.startBackend()
	return f
}

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}

func killProcess(p *exec.Cmd) {
	if p != nil && p.Process != nil {
		_ = p.Process.Kill()
		_ = p.Wait()
	}
}

func (f *fixture) command(path string, args []string, logName string) *exec.Cmd {
	f.t.Helper()
	out, err := os.OpenFile(filepath.Join(f.dir, logName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = out.Close() })
	p := exec.Command(path, args...)
	p.Stdout, p.Stderr = out, out
	return p
}

func (f *fixture) startBackend() {
	f.t.Helper()
	f.backendRuns++
	p := f.command(os.Args[0], []string{"-test.run=^TestReliableBackendProcess$", "-test.v"}, fmt.Sprintf("backend-%d.log", f.backendRuns))
	p.Env = append(os.Environ(), "INFRAPULSE_E2E_BACKEND_CHILD=1", "INFRAPULSE_E2E_DB="+f.dbURL, "INFRAPULSE_E2E_DIR="+f.dir, "INFRAPULSE_E2E_GRPC="+f.grpc, "INFRAPULSE_E2E_HTTP="+f.http)
	if err := p.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.backend = p
	f.wait("Backend ready", 15*time.Second, func() bool {
		c := http.Client{Timeout: time.Second}
		r, err := c.Get("http://" + f.http + "/readyz")
		if err != nil {
			return false
		}
		_ = r.Body.Close()
		return r.StatusCode == 200
	})
}

func (f *fixture) startAgent(mode string) {
	f.t.Helper()
	f.agentRuns++
	// fast_interval is intentionally omitted: test the actual 3s default.
	config := fmt.Sprintf("agent:\n  name: reliable-linux-e2e\n  state_dir: %s\ncontroller:\n  address: %s\n  reconnect_initial: 100ms\n  reconnect_max: 500ms\nsecurity:\n  mode: development\nlocal:\n  enabled: true\ntelemetry:\n  inventory_interval: 3s\n  heartbeat_interval: 2s\nqueue:\n  mode: %s\n  ack_fallback: reject\n  ack_timeout: 15s\nlogging:\n  level: debug\ncommands:\n  enabled: false\n", filepath.Join(f.dir, "agent-state"), f.grpc, mode)
	if err := os.WriteFile(f.config, []byte(config), 0600); err != nil {
		f.t.Fatal(err)
	}
	p := f.command(os.Getenv("AGENT_BINARY"), []string{"run", "--config", f.config}, fmt.Sprintf("agent-%d.log", f.agentRuns))
	if err := p.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.agent = p
}

func (f *fixture) wait(label string, timeout time.Duration, condition func() bool) {
	f.t.Helper()
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatalf("timed out waiting for %s; artifacts: %s", label, f.dir)
}

func (f *fixture) events() []wireEvent {
	f.t.Helper()
	p, err := os.Open(filepath.Join(f.dir, "wire.jsonl"))
	if err != nil {
		return nil
	}
	defer p.Close()
	var events []wireEvent
	s := bufio.NewScanner(p)
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		var e wireEvent
		if json.Unmarshal(s.Bytes(), &e) == nil {
			events = append(events, e)
		}
	}
	return events
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) fault(mode string) {
	f.t.Helper()
	b, _ := json.Marshal(ackFault{Mode: mode, Kind: "telemetry"})
	if err := os.WriteFile(filepath.Join(f.dir, "fault.json"), b, 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) faultHit() wireEvent {
	f.t.Helper()
	var e wireEvent
	f.wait("post-commit ACK fault", 20*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(f.dir, "fault-hit.json"))
		return err == nil && json.Unmarshal(b, &e) == nil && e.Committed == 1
	})
	return e
}

func (f *fixture) waitACK(id string) {
	f.t.Helper()
	f.wait("ACK for "+id, 25*time.Second, func() bool {
		for _, e := range f.events() {
			if e.Type == "ack" && e.MessageID == id && e.Committed == 1 {
				return true
			}
		}
		return false
	})
}

func (f *fixture) replay(id string, newSession bool) {
	f.t.Helper()
	f.waitACK(id)
	var attempts []wireEvent
	for _, e := range f.events() {
		if e.Type == "receive" && e.MessageID == id {
			attempts = append(attempts, e)
		}
	}
	if len(attempts) < 2 {
		f.t.Fatalf("no replay for %s: %+v", id, attempts)
	}
	a, b := attempts[0], attempts[len(attempts)-1]
	if !b.SentAt.After(a.SentAt) || a.AgentID != b.AgentID || (newSession && a.SessionID == b.SessionID) || (!newSession && a.SessionID != b.SessionID) {
		f.t.Fatalf("incorrect replay attempt semantics: first=%+v last=%+v", a, b)
	}
	if !newSession && b.Sequence <= a.Sequence {
		f.t.Fatalf("same-process replay did not allocate a new sequence: %d -> %d", a.Sequence, b.Sequence)
	}
	n := f.count(`SELECT count(*) FROM telemetry WHERE agent_id=$1 AND message_id=$2`, a.AgentID, id)
	if n != 1 {
		f.t.Fatalf("duplicate domain effect: %d rows", n)
	}
	f.drained("durable")
	f.t.Logf("message=%s receive_attempts=%d telemetry_rows_before=1 telemetry_rows_after=%d sequence=%d->%d session_changed=%t ACK_committed=true", id, len(attempts), n, a.Sequence, b.Sequence, a.SessionID != b.SessionID)
}

func (f *fixture) drained(mode string) {
	f.t.Helper()
	since := time.Now().UTC()
	// Heartbeat captures queue state before adding its own entry. A later zero
	// snapshot proves every prior logical message (including the replay) left
	// both pending records and inflight leases without stopping collectors.
	f.wait("zero pending/unacked queue snapshot", 20*time.Second, func() bool {
		for _, e := range f.events() {
			if e.Type != "receive" || e.Kind != "heartbeat" || e.At.Before(since) {
				continue
			}
			var envelope pb.AgentEnvelope
			if protojson.Unmarshal(e.Payload, &envelope) != nil {
				continue
			}
			h := envelope.GetHeartbeat()
			if h.GetTimestamp() == nil || h.Timestamp.AsTime().Before(since) {
				// A newly received replay can carry a queue snapshot captured
				// before the target ACK; its transport time is insufficient.
				continue
			}
			q := h.GetQueue()
			if q.GetMode() == mode && q.Messages == 0 && q.Unacked == 0 && q.StorageError == "" && q.DroppedTelemetry == 0 && q.DroppedOther == 0 && q.DroppedLogs == 0 {
				// Require this observation after the most recent delivered ACK.
				lastACK := time.Time{}
				for _, other := range f.events() {
					if other.Type == "ack" && other.At.After(lastACK) && other.At.Before(e.At) {
						lastACK = other.At
					}
				}
				if !lastACK.IsZero() {
					return true
				}
			}
		}
		return false
	})
	var diag struct {
		Platform    string    `json:"platform"`
		Connected   bool      `json:"connected"`
		ACKFallback bool      `json:"ack_fallback"`
		ObservedAt  time.Time `json:"observed_at"`
		Queue       struct {
			Mode             string `json:"mode"`
			StorageError     string `json:"storage_error"`
			Unacked          int    `json:"unacked"`
			DroppedTelemetry uint64 `json:"dropped_telemetry"`
			DroppedOther     uint64 `json:"dropped_other"`
			DroppedLogs      uint64 `json:"dropped_logs"`
		} `json:"queue"`
	}
	f.wait("persisted Linux diagnostics", 10*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(f.dir, "agent-state", "diagnostics.json"))
		return err == nil && json.Unmarshal(b, &diag) == nil && !diag.ObservedAt.Before(since) && diag.Connected && diag.Platform == "linux/amd64" && diag.Queue.Mode == mode && !diag.ACKFallback && diag.Queue.StorageError == "" && diag.Queue.DroppedTelemetry == 0 && diag.Queue.DroppedOther == 0 && diag.Queue.DroppedLogs == 0
	})
	f.t.Logf("fresh diagnostics platform=%s queue.mode=%s ack_fallback=%t; post-ACK heartbeat queue pending=0 unacked=0 dropped=0", diag.Platform, diag.Queue.Mode, diag.ACKFallback)
}

func (f *fixture) get(path string) json.RawMessage {
	f.t.Helper()
	r, err := http.Get("http://" + f.http + path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil || r.StatusCode != 200 || !json.Valid(b) {
		f.t.Fatalf("REST %s: status=%d body=%s err=%v", path, r.StatusCode, b, err)
	}
	fileName := strings.NewReplacer("/", "_", "?", "_", "=", "_", "&", "_").Replace(path)
	_ = os.WriteFile(filepath.Join(f.dir, fileName+".json"), b, 0600)
	return b
}

func (f *fixture) verifyLive(cadence bool) {
	f.t.Helper()
	minimum := 3
	if cadence {
		minimum = 8
	}
	f.wait("real Linux telemetry/heartbeat/inventory", 35*time.Second, func() bool {
		return f.count(`SELECT count(*) FROM telemetry WHERE source='local'`) >= minimum && f.count(`SELECT count(*) FROM agent_messages WHERE kind='heartbeat'`) >= 2 && f.count(`SELECT count(*) FROM agent_messages WHERE kind='inventory'`) >= 2
	})
	var agentID, deviceID string
	if err := f.db.QueryRow(context.Background(), `SELECT agent_id,device_id FROM telemetry WHERE source='local' LIMIT 1`).Scan(&agentID, &deviceID); err != nil {
		f.t.Fatal(err)
	}
	devices := f.get("/api/devices")
	if !strings.Contains(string(devices), deviceID) || !strings.Contains(string(devices), agentID) {
		f.t.Fatalf("REST devices missing real identity: %s", devices)
	}
	latest := f.get("/api/devices/" + url.PathEscape(deviceID) + "/telemetry?agent_id=" + url.QueryEscape(agentID))
	var telemetry struct {
		Sample json.RawMessage `json:"sample"`
	}
	if err := json.Unmarshal(latest, &telemetry); err != nil {
		f.t.Fatal(err)
	}
	var sample pb.TelemetrySample
	if err := protojson.Unmarshal(telemetry.Sample, &sample); err != nil || sample.Memory == nil || sample.Memory.TotalBytes == 0 || sample.Cpu == nil || sample.Cpu.UsagePercent == nil || len(sample.Interfaces) == 0 {
		f.t.Fatalf("REST missing CPU/RAM/interface collector values: %s error=%v", latest, err)
	}
	hasCountersAndRates := false
	for _, iface := range sample.Interfaces {
		if iface.Counters != nil && iface.Rates != nil {
			hasCountersAndRates = true
		}
	}
	if !hasCountersAndRates {
		f.t.Fatalf("REST missing real interface counters/rates: %s", latest)
	}
	f.get("/api/devices/" + url.PathEscape(deviceID) + "/metrics/history?agent_id=" + url.QueryEscape(agentID))
	f.get("/api/devices/" + url.PathEscape(deviceID) + "/inventory?agent_id=" + url.QueryEscape(agentID))
	f.t.Logf("REST device=%s agent=%s CPU=%.3f RAM_total=%d interfaces=%d Linux samples=%d", deviceID, agentID, *sample.Cpu.UsagePercent, sample.Memory.TotalBytes, len(sample.Interfaces), f.count(`SELECT count(*) FROM telemetry WHERE source='local'`))
	if cadence {
		rows, err := f.db.Query(context.Background(), `SELECT observed_at FROM telemetry WHERE source='local' ORDER BY observed_at LIMIT 8`)
		if err != nil {
			f.t.Fatal(err)
		}
		var times []time.Time
		for rows.Next() {
			var ts time.Time
			if err := rows.Scan(&ts); err != nil {
				f.t.Fatal(err)
			}
			times = append(times, ts)
		}
		rows.Close()
		if rows.Err() != nil {
			f.t.Fatal(rows.Err())
		}
		var intervals []float64
		for i := 1; i < len(times); i++ {
			delta := times[i].Sub(times[i-1]).Seconds()
			intervals = append(intervals, delta)
			if delta < 2.9 || delta > 3.1 {
				f.t.Fatalf("3s default cadence changed: %.6fs", delta)
			}
		}
		f.t.Logf("actual default telemetry intervals_seconds=%v", intervals)
	}
}

func TestReliableLinuxEndToEnd(t *testing.T) {
	t.Run("normal_durable_and_REST", func(t *testing.T) {
		f := newFixture(t)
		f.startAgent("durable")
		f.verifyLive(true)
		f.drained("durable")
	})
	t.Run("lost_ACK_replay", func(t *testing.T) {
		f := newFixture(t)
		f.fault("drop")
		f.startAgent("durable")
		hit := f.faultHit()
		f.replay(hit.MessageID, false)
	})
	t.Run("Backend_process_restart_after_COMMIT", func(t *testing.T) {
		f := newFixture(t)
		f.fault("hold")
		f.startAgent("durable")
		hit := f.faultHit()
		if n := f.count(`SELECT count(*) FROM telemetry WHERE message_id=$1`, hit.MessageID); n != 1 {
			t.Fatalf("pre-restart commit missing: %d", n)
		}
		killProcess(f.backend)
		f.backend = nil
		_ = os.Remove(filepath.Join(f.dir, "fault.json"))
		f.startBackend()
		f.replay(hit.MessageID, false)
		f.verifyLive(false)
	})
	t.Run("Agent_process_restart_new_session", func(t *testing.T) {
		f := newFixture(t)
		f.fault("hold")
		f.startAgent("durable")
		hit := f.faultHit()
		f.wait("unacked durable diagnostics", 5*time.Second, func() bool {
			b, err := os.ReadFile(filepath.Join(f.dir, "agent-state", "diagnostics.json"))
			var d struct {
				Queue struct{ Messages, Unacked int } `json:"queue"`
			}
			return err == nil && json.Unmarshal(b, &d) == nil && d.Queue.Messages > 0 && d.Queue.Unacked > 0
		})
		killProcess(f.agent)
		f.agent = nil
		_ = os.Remove(filepath.Join(f.dir, "fault.json"))
		f.startAgent("durable")
		f.replay(hit.MessageID, true)
		if n := f.count(`SELECT count(*) FROM agent_sessions WHERE agent_id=$1`, hit.AgentID); n != 2 {
			t.Fatalf("process restart session history=%d", n)
		}
	})
	t.Run("DB_rollback_retains_then_recovers", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.db.Exec(context.Background(), `ALTER TABLE telemetry ADD CONSTRAINT fail_delivery CHECK(false) NOT VALID`); err != nil {
			t.Fatal(err)
		}
		f.startAgent("durable")
		var rejected wireEvent
		f.wait("telemetry rejected by PostgreSQL", 10*time.Second, func() bool {
			for _, e := range f.events() {
				if e.Type == "receive" && e.Kind == "telemetry" {
					rejected = e
					return true
				}
			}
			return false
		})
		f.wait("rollback/retry without ACK", 5*time.Second, func() bool {
			n := 0
			for _, e := range f.events() {
				if e.MessageID == rejected.MessageID {
					if e.Type == "ack" {
						t.Fatal("database failure was ACKed")
					}
					if e.Type == "receive" {
						n++
					}
				}
			}
			return n >= 2
		})
		if n := f.count(`SELECT count(*) FROM agent_messages WHERE message_id=$1`, rejected.MessageID); n != 0 {
			t.Fatalf("rollback left inbox row: %d", n)
		}
		if n := f.count(`SELECT count(*) FROM telemetry WHERE message_id=$1`, rejected.MessageID); n != 0 {
			t.Fatalf("rollback left domain row: %d", n)
		}
		if _, err := f.db.Exec(context.Background(), `ALTER TABLE telemetry DROP CONSTRAINT fail_delivery`); err != nil {
			t.Fatal(err)
		}
		f.waitACK(rejected.MessageID)
		if n := f.count(`SELECT count(*) FROM telemetry WHERE message_id=$1`, rejected.MessageID); n != 1 {
			t.Fatalf("recovery domain rows: %d", n)
		}
		f.drained("durable")
		t.Logf("message=%s rollback inbox=0 telemetry=0 ACK=none; recovered telemetry=1 ACK=post-commit", rejected.MessageID)
	})
	t.Run("memory_regression", func(t *testing.T) {
		f := newFixture(t)
		f.startAgent("memory")
		f.verifyLive(false)
		f.drained("memory")
	})
}

func TestReliableWireConflictAndP3Archive(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(f.grpc, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := pb.NewAgentServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := timestamppb.Now()
	base := &pb.AgentEnvelope{AgentId: "archive-agent", SessionId: "archive-session", MessageId: "archive-hello", Sequence: 1, SentAt: now, Payload: &pb.AgentEnvelope_Hello{Hello: &pb.AgentHello{AgentId: "archive-agent", SessionId: "archive-session", ProtocolVersion: 1, ApplicationAck: true, StartedAt: now, Device: &pb.Device{Id: "archive-device", Source: "fixture"}}}}
	if err := stream.Send(base); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil || !ack.GetHelloAck().GetApplicationAck() {
		t.Fatalf("P3 hello: %v %v", ack, err)
	}
	// Hello is confirmed by HelloAck. Some controllers additionally ACK its ID;
	// consume either behavior without hiding any application ACK failure.
	sendAndACK := func(e *pb.AgentEnvelope) {
		t.Helper()
		if err := stream.Send(e); err != nil {
			t.Fatal(err)
		}
		for {
			ack, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if ack.GetMessageAck().GetMessageId() == e.MessageId {
				return
			}
			if ack.GetMessageAck().GetMessageId() != base.MessageId {
				t.Fatalf("unexpected ACK: %v", ack)
			}
		}
	}
	heartbeat := proto.Clone(base).(*pb.AgentEnvelope)
	heartbeat.MessageId, heartbeat.Sequence = "archive-heartbeat", 2
	heartbeat.Payload = &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{Timestamp: now, Queue: &pb.QueueState{Mode: "durable", StorageBytes: 12345, OldestPending: now, Unacked: 2, PendingResults: 1, StorageError: "fixture"}, Certificate: &pb.CertificateStatus{ExpiresAt: now, RemainingSeconds: 123, EnrollmentState: "enrolled", RenewalState: "ready", LastSuccessfulRenewal: now, LastError: "fixture"}}}
	sendAndACK(heartbeat)
	result := proto.Clone(base).(*pb.AgentEnvelope)
	result.MessageId, result.Sequence = "archive-command-result", 3
	result.Payload = &pb.AgentEnvelope_CommandResult{CommandResult: &pb.CommandResult{CommandId: "fixture-command", SnapshotId: "snapshot-preserved"}}
	sendAndACK(result)
	var raw []byte
	if err := f.db.QueryRow(context.Background(), `SELECT payload FROM agent_messages WHERE message_id=$1`, heartbeat.MessageId).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var recovered pb.AgentEnvelope
	if err := protojson.Unmarshal(raw, &recovered); err != nil || !proto.Equal(heartbeat.GetHeartbeat(), recovered.GetHeartbeat()) {
		t.Fatalf("P3 heartbeat fields lost: %s err=%v", raw, err)
	}
	if err := f.db.QueryRow(context.Background(), `SELECT payload FROM agent_messages WHERE message_id=$1`, result.MessageId).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := protojson.Unmarshal(raw, &recovered); err != nil || recovered.GetCommandResult().GetSnapshotId() != "snapshot-preserved" {
		t.Fatalf("snapshot_id lost: %s err=%v", raw, err)
	}
	conflict := proto.Clone(heartbeat).(*pb.AgentEnvelope)
	conflict.Sequence, conflict.SentAt = 4, timestamppb.Now()
	conflict.GetHeartbeat().Queue.Mode = "changed-business-payload"
	if err := stream.Send(conflict); err != nil {
		t.Fatal(err)
	}
	ack, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument || ack != nil {
		t.Fatalf("same-ID changed payload must reject without ACK: %v %v", ack, err)
	}
	t.Log("P3 QueueState/CertificateStatus/snapshot_id archive survived decode; conflict InvalidArgument without ACK")
}
