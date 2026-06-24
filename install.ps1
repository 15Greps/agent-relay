<#
.SYNOPSIS
    agent-relay installer for Windows (PowerShell)
.DESCRIPTION
    Downloads and installs the agent-relay binary to %USERPROFILE%\.local\bin
.EXAMPLE
    powershell -ExecutionPolicy Bypass -Command "irm https://relay.agentforms.io/install.ps1 | iex"
#>

param(
    [string]$Version = "0.2.6",
    [string]$InstallDir = "$env:USERPROFILE\.local\bin"
)

$ErrorActionPreference = "Stop"

$RepoUrl = "https://github.com/15Greps/agent-relay/releases/download/v$Version"
$Binary = "relay-windows-amd64.exe"
$Url = "$RepoUrl/$Binary"

Write-Host "agent-relay v$Version installer" -ForegroundColor Green
Write-Host "Platform: windows-amd64"
Write-Host "Install dir: $InstallDir"
Write-Host ""

# Create install directory
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
}

# Download
$TargetPath = Join-Path $InstallDir "relay.exe"
Write-Host "Downloading $Binary..."

try {
    # Try Invoke-WebRequest first (PowerShell 3+)
    Invoke-WebRequest -Uri $Url -OutFile $TargetPath -UseBasicParsing
} catch {
    Write-Error "Failed to download: $_"
    exit 1
}

# Verify
try {
    $result = & $TargetPath version 2>&1
    if ($LASTEXITCODE -eq 0) {
        Write-Host ""
        Write-Host "✓ Installed relay.exe to $TargetPath" -ForegroundColor Green
        Write-Host ""

        # Check PATH
        $pathDirs = $env:PATH.Split(';')
        if ($pathDirs -notcontains $InstallDir) {
            Write-Host "Add $InstallDir to your PATH:" -ForegroundColor Yellow
            Write-Host "  [Environment]::SetEnvironmentVariable('PATH', `$env:PATH + ';$InstallDir', 'User')"
            Write-Host ""
            Write-Host "Or manually:"
            Write-Host "  Right-click Start → System → Advanced → Environment Variables"
            Write-Host "  Edit Path → New → paste: $InstallDir"
        }
    } else {
        Write-Error "Downloaded binary failed verification"
        Remove-Item $TargetPath -ErrorAction SilentlyContinue
        exit 1
    }
} catch {
    Write-Error "Verification failed: $_"
    Remove-Item $TargetPath -ErrorAction SilentlyContinue
    exit 1
}
