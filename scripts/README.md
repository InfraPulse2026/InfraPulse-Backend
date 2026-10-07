## Protobuf generation

The Agent's `api/proto/infrapulse/v1/agent.proto` is canonical. The Backend copy
differs only in `go_package`, which points at the Backend Go module. Existing
field numbers and enum values must stay compatible with the frozen P2 descriptor
tested by `go test ./api/proto`.

Generation pins protoc **33.5**, protoc-gen-go **v1.36.12**, and
protoc-gen-go-grpc **v1.6.0**. Obtain protoc from the official protobuf 33.5
release; install plugins with:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.0
```

Use `scripts/generate-proto.ps1` on Windows or `sh scripts/generate-proto.sh`
on Linux. Each script verifies exact tool versions before generating both Go
files. It searches Backend `.tools`, sibling Agent `.tools`, then `PATH`.
Override locations with `PROTOC`, `PROTOC_INCLUDE`, `PROTOC_GEN_GO`, and
`PROTOC_GEN_GO_GRPC`.

To verify canonical contract drift and reproducible generation without changing
checked-in files:

```powershell
./scripts/generate-proto.ps1 -Check
```

```sh
sh scripts/generate-proto.sh --check
```

The check requires the canonical Agent checkout. Its default location is sibling
`../InfraPulseAgent`; override with PowerShell `-AgentRoot` or shell `AGENT_ROOT`.
Git line-ending conversion is tolerated; any other difference fails the check.
