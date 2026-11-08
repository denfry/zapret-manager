# Local quality gate — the same checks as .github/workflows/ci.yml.
# Usage (from the repo root):  powershell -ExecutionPolicy Bypass -File scripts\check.ps1 [-Fuzz 15s] [-SkipTools]
#   -Fuzz      fuzz time per target (default 10s, "0" to skip)
#   -SkipTools skip staticcheck / golangci-lint / govulncheck (offline machine)
# The race detector needs cgo (gcc in PATH); without gcc it is skipped with a notice.
param(
    [string]$Fuzz = "10s",
    [switch]$SkipTools
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

$golangciVersion = "v2.15.0"
$staticcheckVersion = "v0.8.2"
$govulncheckVersion = "v1.9.0"

function Step($name, [scriptblock]$body) {
    Write-Host "==> $name" -ForegroundColor Cyan
    & $body
    if ($LASTEXITCODE -ne 0) { Write-Host "FAILED: $name" -ForegroundColor Red; exit $LASTEXITCODE }
}

Step "gofmt" {
    $files = gofmt -l .
    if ($files) { Write-Host "Not gofmt-ed:"; $files; $global:LASTEXITCODE = 1 } else { $global:LASTEXITCODE = 0 }
}
Step "go mod verify" { go mod verify }
Step "go mod tidy -diff" { go mod tidy -diff }
Step "go vet" { go vet ./... }

if (-not $SkipTools) {
    Step "golangci-lint $golangciVersion" { go run "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$golangciVersion" run ./... }
    Step "staticcheck $staticcheckVersion" { go run "honnef.co/go/tools/cmd/staticcheck@$staticcheckVersion" ./... }
}

if (Get-Command gcc -ErrorAction SilentlyContinue) {
    $env:CGO_ENABLED = "1"
    Step "go test -race" { go test -race -count=1 -cover ./... }
} else {
    Write-Host "gcc not found: race detector skipped (CI runs it)" -ForegroundColor Yellow
    Step "go test" { go test -count=1 -cover ./... }
}

if ($Fuzz -ne "0") {
    $targets = @(
        @("./internal/batch", "FuzzExtractWinwsArgs"),
        @("./internal/batch", "FuzzExeFromCmdline"),
        @("./internal/batch", "FuzzUnescapeCarets"),
        @("./internal/hostsfile", "FuzzMerge"),
        @("./internal/hostsfile", "FuzzParseUpstream"),
        @("./internal/safezip", "FuzzEntryPath"),
        @("./internal/sitelist", "FuzzRender"),
        @("./internal/sitelist", "FuzzNormalizeDomain"),
        @("./internal/version", "FuzzCompare"),
        @("./internal/zapretfmt", "FuzzParseGameFilter")
    )
    foreach ($t in $targets) {
        Step "fuzz $($t[1]) ($Fuzz)" { go test -run '^$' -fuzz "^$($t[1])$" -fuzztime $Fuzz $t[0] }
    }
}

if (-not $SkipTools) {
    Step "govulncheck $govulncheckVersion" { go run "golang.org/x/vuln/cmd/govulncheck@$govulncheckVersion" ./... }
}

Step "build" { go build -trimpath -ldflags "-s -w" -o dist\zapret-manager.exe . }
Write-Host "OK: dist\zapret-manager.exe" -ForegroundColor Green
