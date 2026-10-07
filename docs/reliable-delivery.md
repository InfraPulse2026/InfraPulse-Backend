# Agent P3 → Backend reliable delivery

Canonical contract: sibling `InfraPulseAgent/api/proto/infrapulse/v1/agent.proto`.
Backend differs only in its Go package option. Agent runtime, scheduler, stable
message IDs and durable outbox were not changed for this integration fix.

## Acceptance and replay

Hello with `application_ack=true` receives an accepted protocol-v1 HelloAck with
`application_ack=true` after Agent/session/device/inbox registration commits.
Hello itself is outside the durable queue and is confirmed by HelloAck.
Clients that omit the capability keep the previous handshake and receive no
additional MessageACKs.

For every supported non-Hello envelope:

```text
receive → validate → BEGIN → session tracking → inbox insert/dedup
        → domain persistence (new message only) → COMMIT → MessageACK(message_id)
```

`Store.Ingest` returns success only after COMMIT. Its inbox and domain effects
remain in one transaction. A duplicate updates session tracking, skips domain
effects, commits, then receives ACK. SQL failures, COMMIT failures, validation
errors and conflicting reuse receive no ACK. Failed ACK delivery leaves the
committed row available for the Agent's next replay. All stream sends use one
serialized path.

Identity is `(agent_id,message_id)`. The existing JSONB archive comparison also
requires the same payload kind and stable contents, excluding **only outer**
`sequence`, `sent_at`, `session_id`. Nested business timestamps and payload
fields remain significant. No schema change, backfill or change to old
migrations is required; existing rows use the same comparison.

Known archive-only messages (topology/logs/events/telephony/results/errors) get
the same post-commit ACK. This adds no domain projections or command dispatcher.
Unknown protobuf fields/payloads are rejected before JSON serialization rather
than acknowledged with their contents lost.

## Reproduce

Use a separate PostgreSQL test database. Tests create and drop isolated schemas.
With sibling repositories available:

```sh
export TEST_DATABASE_URL='postgres://testuser:testpass@127.0.0.1:5432/infrapulse_test?sslmode=disable'
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
go mod verify
sh scripts/generate-proto.sh --check
```

Pinned generator setup and Windows `-Check` are documented in
[scripts/README.md](../scripts/README.md). Wire compatibility tests include the
frozen P2 descriptor, canonical schema comparison and P3 archive roundtrips.

On Linux, build the **real Agent** and provide its absolute executable path:

```sh
(cd ../InfraPulseAgent && CGO_ENABLED=0 go build -o bin/infrapulse-agent ./cmd/infrapulse-agent)
export AGENT_BINARY="$(cd ../InfraPulseAgent && pwd)/bin/infrapulse-agent"
export E2E_ARTIFACTS="$PWD/bin/reliable-e2e/results"
go test ./internal/integration -count=1 -v -timeout=4m
```

For a Windows workspace with Linux runtime, crosscompile both executables with
`GOOS=linux GOARCH=amd64 CGO_ENABLED=0`: Agent `go build -o <agent-linux>` and
Backend `go test -c -o bin/reliable-e2e/e2e.test-linux ./internal/integration`.
Run the test executable in Linux with the same environment variables and a
PostgreSQL URL reachable there. In this workspace the WSL distro is
`docker-desktop`, and workspace paths start with `/mnt/host/c/`.

The E2E helper starts a separate Backend process using the actual production
Controller, Store, migrations and REST handler. A test-only stream wrapper
observes messages and injects lost/held ACKs; production code has no fault
injection flags. A separate PostgreSQL connection verifies registration and
all telemetry domain rows are externally visible before ACK. Process restart
uses SIGKILL. With E2E_ARTIFACTS, process logs, wire JSONL, diagnostics and REST
responses remain after temporary database schemas are removed.

## Verified on 6 October 2026

Go 1.27.1; PostgreSQL 18 in a new isolated Windows cluster; real Linux collector
under WSL (Linux amd64). Runtime E2E passed, including the final stronger
post-commit assertions. Agent uses durable mode with `ack_fallback: reject`.
The test omits `fast_interval`, exercising the actual 3s default. Heartbeat and
inventory are accelerated to 2s/3s solely in test configuration.

| Scenario | Result |
|---|---|
| Hello → full registration COMMIT → HelloAck | PASS, application_ack=true |
| Normal heartbeat, inventory, telemetry → COMMIT → ACK | PASS, later heartbeat pending=0/unacked=0 |
| Lost ACK, reconnect, same-ID replay | PASS, sequence 6→10, telemetry rows 1→1 |
| Backend SIGKILL after COMMIT before ACK | PASS, same process session, sequence 6→10, rows 1→1 |
| Agent SIGKILL with unacked outbox → recovery | PASS, persistent agent_id, new session_id, rows 1→1, queue drained |
| PostgreSQL statement failure → reconnect/retry | PASS, inbox/domain 0/0, no ACK; same ID recovered to 1/1 and ACK |
| Deferred COMMIT failure (storage integration) | PASS, inbox/domain 0/0; retry after recovery 1/1 |
| Same ID with changed business contents/kind | PASS, conflict/InvalidArgument, no ACK |
| Eight concurrent legitimate replay attempts | PASS, one inbox row and one telemetry effect |
| Memory mode + inventory + real Linux REST | PASS |
| P3 queue/certificate/snapshot_id wire → JSONB | PASS, complete fields preserved |

Final measured telemetry intervals (seconds):
`2.998009, 3.001399, 2.999151, 3.000257, 2.999351, 3.000600, 2.999201`.
REST `/api/devices`, telemetry, metrics history and inventory returned the real
device `local:8ff980d6-fda5-48fa-aaf2-8575c2c4f0e3`, CPU usage 0.250%, RAM total
16,647,856,128 bytes and two interfaces with counters/rates. Fresh heartbeat
business timestamps and diagnostics observation timestamps confirm queue drain
after the target ACK; pending, unacked and all dropped counters are zero.

A separate smoke run used the ordinary `cmd/server` Linux executable and the
real Agent, without the fault wrapper. PASS: application_ack=true, five inventory
updates, seven heartbeats, five distinct telemetry messages/five telemetry rows,
real REST CPU/RAM/two interfaces. Intervals were
`2.999815, 2.999276, 3.000816, 2.999904` seconds. Six later heartbeat snapshots
show durable queue messages/unacked=0. Its artifacts are
`bin/reliable-e2e/server-smoke-*.json` and process logs.

## Full regression results

All final checks below passed. Backend PostgreSQL tests used TEST_DATABASE_URL
and ran against real PostgreSQL, rather than being skipped.

| Command | Backend | Agent |
|---|---|---|
| `go build ./...` | PASS (Windows and Linux cross-build) | PASS; Linux executable cross-built |
| `go vet ./...` | PASS (Windows and Linux variants) | PASS |
| `go test ./... -count=1` | PASS (Windows; Linux via WSL exec wrapper) | PASS (full native Linux execution via WSL wrapper, `-p 1`) |
| `go test -race ./... -count=1` | PASS, Windows with portable mingw | PASS, Windows with portable mingw and `-p 1` |
| `go mod verify` | PASS | PASS |
| `go test ./internal/storage -count=1 -v` | PASS, real PostgreSQL including statement/COMMIT rollback | — |
| `go test -race ./internal/storage -count=1` | PASS, real PostgreSQL | — |
| `scripts/generate-proto.ps1 -Check` | PASS | canonical schema read-only |
| `sh scripts/generate-proto.sh --check` | PASS (Git Bash) | canonical schema read-only |
| `sh -n scripts/generate-proto.sh` | PASS | — |
| changed Go files: gofmt; `git diff --check` | PASS | no source changes |
| real Linux Agent `validate-config` for durable example | PASS | same unmodified executable |
| `e2e.test-linux -test.v -test.timeout=4m` | PASS, real Linux Agent + separate Backend processes + PostgreSQL + REST | PASS |

The Windows host has no Go/C toolchain inside WSL. A disposable executable in
ignored `bin/agent-test-linux-exec.exe` translates Windows test-binary paths to
`/mnt/host/c/...` and invokes WSL with TMPDIR=/tmp. Exact native-Linux regression
invocation from Windows Go: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -exec
<wrapper> ./... -count=1` (`-p 1` for Agent). This executes the complete test suite,
including Linux build variants; it is not a reduced subset.

Earlier ordinary Windows Agent runs failed on SNMP/RouterOS fixture deadline
timing and intermittent TempDir removal. The complete final Linux ordinary and
Windows race runs passed. No Agent source, delivery semantics or tests were
modified to hide these host-specific failures. An initial unprivileged Linux
launch was denied WSL access before executing tests; the authorized launch passed.

Durable delivery remains subject to configured Agent outbox bounds/retention and
the durability of PostgreSQL itself. The Backend remains a single Controller
instance. Enrollment/certificate-to-agent identity, new projections, command
dispatch, ConfigUpdate and other roadmap stages remain outside this fix.
