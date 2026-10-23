# zMail build helper for Windows PowerShell (no GNU make required).
# Usage:
#   .\make.ps1 build      # static build -> zmail.exe (CGO_ENABLED=0)
#   .\make.ps1 check      # build + vet
#   .\make.ps1 vet|fmt|run|tidy|clean|genkey|build-linux
param(
  [Parameter(Position = 0)]
  [string]$Target = 'build'
)

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $MyInvocation.MyCommand.Path)

function Test-Go {
  if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host '[error] go not found. Install Go 1.26+ and add %LOCALAPPDATA%\Programs\Go\bin to PATH.' -ForegroundColor Red
    Write-Host '        Download: https://go.dev/dl/' -ForegroundColor Yellow
    exit 1
  }
}

function Invoke-Go {
  param([string[]]$GoArgs)
  & go @GoArgs
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

Test-Go

$version = 'dev'; $oldEap = $ErrorActionPreference; $ErrorActionPreference = 'SilentlyContinue'; $described = (git describe --tags --always 2>$null | Select-Object -First 1); if ($described) { $version = "$described".Trim() }; $ErrorActionPreference = $oldEap; $ldflags = "-s -w -X main.version=$version"

switch ($Target) {
  'build-linux' {
    $oldGOOS=$env:GOOS; $oldGOARCH=$env:GOARCH; $env:CGO_ENABLED='0'; $env:GOOS='linux'
    New-Item -ItemType Directory -Force 'dist' | Out-Null
    try {
      $env:GOARCH='amd64'
      Invoke-Go @('build', '-trimpath', '-ldflags', $ldflags, '-o', 'dist/zmail-linux-amd64', './cmd/zmail')
      $env:GOARCH='arm64'
      Invoke-Go @('build', '-trimpath', '-ldflags', $ldflags, '-o', 'dist/zmail-linux-arm64', './cmd/zmail')
    } finally {
      $env:GOOS=$oldGOOS; $env:GOARCH=$oldGOARCH
    }
    Write-Host '[ok] built dist/zmail-linux-amd64 and dist/zmail-linux-arm64' -ForegroundColor Green
  }
  'build' {
    $env:CGO_ENABLED = '0'
    Invoke-Go @('build', '-trimpath', '-ldflags', $ldflags, '-o', 'zmail.exe', './cmd/zmail')
    Write-Host '[ok] built .\zmail.exe' -ForegroundColor Green
  }
  'vet' {
    Invoke-Go @('vet', './...')
  }
  'fmt' {
    & gofmt -l -w .
    Write-Host '[ok] gofmt done' -ForegroundColor Green
  }
  'run' {
    Invoke-Go @('run', './cmd/zmail')
  }
  'tidy' {
    Invoke-Go @('mod', 'tidy')
  }
  'clean' {
    Remove-Item -Force 'zmail.exe' -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force 'dist' -ErrorAction SilentlyContinue
    Write-Host '[ok] cleaned build artifacts' -ForegroundColor Green
  }
  'genkey' {
    Invoke-Go @('run', './cmd/zmail', '-genkey')
  }
  'check' {
    $env:CGO_ENABLED = '0'
    Invoke-Go @('build', '-trimpath', '-ldflags', $ldflags, '-o', 'zmail.exe', './cmd/zmail')
    Invoke-Go @('vet', './...')
    Write-Host '[ok] check passed' -ForegroundColor Green
  }
  'help' {
    Get-Content $MyInvocation.MyCommand.Path | Select-Object -First 8
  }
  default {
    Write-Host ("[error] unknown target: " + $Target) -ForegroundColor Red
    Write-Host 'available: build build-linux vet fmt run tidy clean genkey check'
    exit 1
  }
}
