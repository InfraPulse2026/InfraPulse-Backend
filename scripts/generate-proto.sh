#!/bin/sh
# Requires protoc 33.5, protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.0.
# Install the Go plugins with `go install ...@v1.36.12` and `...@v1.6.0`.
# --check also compares the canonical sibling Agent contract and fresh output.
set -eu

backend_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
agent_root=${AGENT_ROOT:-"$backend_root/../InfraPulseAgent"}
check=false
if [ "${1:-}" = '--check' ]; then check=true; shift; fi
if [ "$#" -ne 0 ]; then echo "Usage: $0 [--check]" >&2; exit 2; fi

resolve_tool() {
    explicit=$1 relative=$2 command_name=$3
    if [ -n "$explicit" ]; then command -v "$explicit"; return; fi
    for tool_root in "$backend_root/.tools" "$agent_root/.tools"; do
        if [ -f "$tool_root/$relative.exe" ] && [ -x "$tool_root/$relative.exe" ]; then printf '%s\n' "$tool_root/$relative.exe"; return; fi
        if [ -f "$tool_root/$relative" ] && [ -x "$tool_root/$relative" ]; then printf '%s\n' "$tool_root/$relative"; return; fi
    done
    command -v "$command_name"
}

protoc=$(resolve_tool "${PROTOC:-}" 'protoc/bin/protoc' 'protoc')
go_plugin=$(resolve_tool "${PROTOC_GEN_GO:-}" 'bin/protoc-gen-go' 'protoc-gen-go')
grpc_plugin=$(resolve_tool "${PROTOC_GEN_GO_GRPC:-}" 'bin/protoc-gen-go-grpc' 'protoc-gen-go-grpc')
[ "$("$protoc" --version | tr -d '\r')" = 'libprotoc 33.5' ] || { echo 'Requires protoc 33.5' >&2; exit 1; }
case "$("$go_plugin" --version | tr -d '\r')" in
    'protoc-gen-go v1.36.12'|'protoc-gen-go.exe v1.36.12') ;;
    *) echo 'Requires protoc-gen-go v1.36.12' >&2; exit 1 ;;
esac
case "$("$grpc_plugin" --version | tr -d '\r')" in
    'protoc-gen-go-grpc 1.6.0'|'protoc-gen-go-grpc v1.6.0'|'protoc-gen-go-grpc.exe 1.6.0') ;;
    *) echo 'Requires protoc-gen-go-grpc v1.6.0' >&2; exit 1 ;;
esac

# Git Bash does not convert the nested path in --plugin=name=/path for protoc.
case "$protoc" in
    *.exe)
        if command -v cygpath >/dev/null 2>&1; then
            go_plugin=$(cygpath -m "$go_plugin")
            grpc_plugin=$(cygpath -m "$grpc_plugin")
        fi
        ;;
esac

proto_root="$backend_root/api/proto"
generated_root="$backend_root/api/gen"
temporary_root=''
cleanup() {
    if [ -n "$temporary_root" ]; then
        rm -f "$temporary_root/agent.proto" "$temporary_root/backend.proto" "$temporary_root/fresh" "$temporary_root/checked-in"
        rm -f "$temporary_root/infrapulse/v1/agent.pb.go" "$temporary_root/infrapulse/v1/agent_grpc.pb.go"
        rmdir "$temporary_root/infrapulse/v1" "$temporary_root/infrapulse" "$temporary_root" 2>/dev/null || true
    fi
}
trap cleanup EXIT HUP INT TERM

if "$check"; then
    temporary_root=$(mktemp -d)
    # go_package is the sole intentional difference between independent repos.
    sed '/^option go_package = "/d; s/\r$//' "$agent_root/api/proto/infrapulse/v1/agent.proto" > "$temporary_root/agent.proto"
    sed '/^option go_package = "/d; s/\r$//' "$proto_root/infrapulse/v1/agent.proto" > "$temporary_root/backend.proto"
    cmp "$temporary_root/agent.proto" "$temporary_root/backend.proto" || { echo 'Backend proto differs from canonical Agent proto beyond go_package' >&2; exit 1; }
    rm -f "$temporary_root/agent.proto" "$temporary_root/backend.proto"
    output_root=$temporary_root
else
    output_root=$generated_root
fi

set -- -I "$proto_root"
include=${PROTOC_INCLUDE:-}
if [ -z "$include" ]; then
    candidate="$(dirname -- "$(dirname -- "$protoc")")/include"
    if [ -d "$candidate" ]; then include=$candidate; fi
fi
if [ -n "$include" ]; then set -- "$@" -I "$include"; fi
"$protoc" "$@" \
    "--plugin=protoc-gen-go=$go_plugin" \
    "--plugin=protoc-gen-go-grpc=$grpc_plugin" \
    "--go_out=$output_root" --go_opt=paths=source_relative \
    "--go-grpc_out=$output_root" --go-grpc_opt=paths=source_relative \
    infrapulse/v1/agent.proto

if "$check"; then
    for file in agent.pb.go agent_grpc.pb.go; do
        # Tolerate Git's platform-specific line endings, but no content drift.
        sed 's/\r$//' "$output_root/infrapulse/v1/$file" > "$temporary_root/fresh"
        sed 's/\r$//' "$generated_root/infrapulse/v1/$file" > "$temporary_root/checked-in"
        cmp "$temporary_root/fresh" "$temporary_root/checked-in" || { echo "$file is not reproducibly generated" >&2; exit 1; }
    done
    rm -f "$temporary_root/fresh" "$temporary_root/checked-in"
    echo 'PASS: canonical Agent contract and reproducible generated Backend code'
fi
