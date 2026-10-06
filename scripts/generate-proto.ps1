param(
    [switch]$Check,
    [string]$AgentRoot = (Join-Path $PSScriptRoot '..\..\InfraPulseAgent'),
    [string]$Protoc = $env:PROTOC,
    [string]$ProtocInclude = $env:PROTOC_INCLUDE,
    [string]$GoPlugin = $env:PROTOC_GEN_GO,
    [string]$GrpcPlugin = $env:PROTOC_GEN_GO_GRPC
)

$ErrorActionPreference = 'Stop'
$backendRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$toolRoots = @((Join-Path $backendRoot '.tools'), (Join-Path $AgentRoot '.tools'))

function Resolve-Tool([string]$ExplicitPath, [string]$RelativePath, [string]$Command) {
    if ($ExplicitPath) { return (Get-Command $ExplicitPath -ErrorAction Stop).Source }
    foreach ($toolRoot in $toolRoots) {
        $candidate = Join-Path $toolRoot $RelativePath
        if (Test-Path -LiteralPath $candidate -PathType Leaf) { return (Resolve-Path $candidate).Path }
    }
    return (Get-Command $Command -ErrorAction Stop).Source
}

$Protoc = Resolve-Tool $Protoc 'protoc\bin\protoc.exe' 'protoc'
$GoPlugin = Resolve-Tool $GoPlugin 'bin\protoc-gen-go.exe' 'protoc-gen-go'
$GrpcPlugin = Resolve-Tool $GrpcPlugin 'bin\protoc-gen-go-grpc.exe' 'protoc-gen-go-grpc'

# Pin all three generators; a version mismatch must not silently rewrite code.
if ((& $Protoc --version).Trim() -ne 'libprotoc 33.5') { throw 'Requires protoc 33.5' }
if ((& $GoPlugin --version).Trim() -notmatch '^protoc-gen-go(?:\.exe)? v1\.36\.12$') { throw 'Requires protoc-gen-go v1.36.12' }
if ((& $GrpcPlugin --version).Trim() -notmatch '^protoc-gen-go-grpc(?:\.exe)? (?:v)?1\.6\.0$') { throw 'Requires protoc-gen-go-grpc v1.6.0' }

$protoRoot = Join-Path $backendRoot 'api\proto'
if ($Check) {
    $canonicalPath = Join-Path $AgentRoot 'api\proto\infrapulse\v1\agent.proto'
    $canonical = [System.IO.File]::ReadAllText($canonicalPath)
    $backend = [System.IO.File]::ReadAllText((Join-Path $protoRoot 'infrapulse\v1\agent.proto'))
    $goPackagePattern = '(?m)^option go_package = "[^"]+";\r?\n'
    if (($canonical -replace $goPackagePattern, '' -replace "`r`n", "`n") -cne ($backend -replace $goPackagePattern, '' -replace "`r`n", "`n")) {
        throw 'Backend proto differs from canonical Agent proto beyond go_package'
    }
}

if (-not $ProtocInclude) {
    $includeCandidate = Join-Path (Split-Path (Split-Path $Protoc -Parent) -Parent) 'include'
    if (Test-Path -LiteralPath $includeCandidate -PathType Container) { $ProtocInclude = $includeCandidate }
}
$generatedRoot = Join-Path $backendRoot 'api\gen'
$temporaryRoot = $null
if ($Check) {
    $temporaryRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('infrapulse-proto-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $temporaryRoot | Out-Null
    $outputRoot = $temporaryRoot
} else { $outputRoot = $generatedRoot }

try {
    $arguments = @('-I', $protoRoot)
    if ($ProtocInclude) { $arguments += @('-I', $ProtocInclude) }
    $arguments += @(
        "--plugin=protoc-gen-go=$GoPlugin",
        "--plugin=protoc-gen-go-grpc=$GrpcPlugin",
        "--go_out=$outputRoot", '--go_opt=paths=source_relative',
        "--go-grpc_out=$outputRoot", '--go-grpc_opt=paths=source_relative',
        'infrapulse/v1/agent.proto'
    )
    & $Protoc @arguments
    if ($LASTEXITCODE -ne 0) { throw 'Protobuf generation failed' }
    if ($Check) {
        foreach ($file in @('agent.pb.go', 'agent_grpc.pb.go')) {
            $relativePath = Join-Path 'infrapulse\v1' $file
            $fresh = [System.IO.File]::ReadAllText((Join-Path $outputRoot $relativePath)).Replace("`r`n", "`n")
            $checkedIn = [System.IO.File]::ReadAllText((Join-Path $generatedRoot $relativePath)).Replace("`r`n", "`n")
            if ($fresh -cne $checkedIn) { throw "$file is not reproducibly generated; run scripts/generate-proto.ps1" }
        }
        Write-Output 'PASS: canonical Agent contract and reproducible generated Backend code'
    }
} finally {
    if ($temporaryRoot) {
        # Only remove the known generated files and their now-empty directories.
        foreach ($file in @('agent.pb.go', 'agent_grpc.pb.go')) {
            Remove-Item -LiteralPath (Join-Path $temporaryRoot "infrapulse\v1\$file") -ErrorAction SilentlyContinue
        }
        Remove-Item -LiteralPath (Join-Path $temporaryRoot 'infrapulse\v1') -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath (Join-Path $temporaryRoot 'infrapulse') -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $temporaryRoot -ErrorAction SilentlyContinue
    }
}
