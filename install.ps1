<#
.SYNOPSIS
    Installs hypervm-mcp and registers it with Claude Code.

.DESCRIPTION
    Downloads the latest release, verifies its checksum, installs the Windows
    service, and registers the MCP server with Claude Code if it is present.

    The service install raises one UAC prompt. That is the whole point: after it,
    Hyper-V is reachable from an unprivileged MCP session without ever prompting
    again.

.EXAMPLE
    irm https://raw.githubusercontent.com/heavycaffeiner/hypervm-mcp/main/install.ps1 | iex

.EXAMPLE
    .\install.ps1 -Version v1.2.0 -SkipClaudeCode
#>
[CmdletBinding()]
param(
    # Release tag to install. Defaults to the latest.
    [string]$Version = 'latest',

    # Skip registering with Claude Code.
    [switch]$SkipClaudeCode,

    # Where to keep the downloaded binary before the service takes its own copy.
    [string]$DownloadPath = (Join-Path $env:TEMP 'hypervm-mcp')
)

$ErrorActionPreference = 'Stop'
$repo = 'heavycaffeiner/hypervm-mcp'

function Write-Step { param([string]$Message) Write-Host "==> $Message" -ForegroundColor Cyan }
function Write-Note { param([string]$Message) Write-Host "    $Message" -ForegroundColor DarkGray }

# --- Preconditions ----------------------------------------------------------

Write-Step 'Checking this machine'

if ($PSVersionTable.PSVersion.Major -lt 5) {
    throw 'PowerShell 5.1 or later is required.'
}

# Hyper-V's PowerShell module is what every operation goes through, so its
# absence is worth catching now rather than at the first tool call.
if (-not (Get-Command Get-VM -ErrorAction SilentlyContinue)) {
    throw @'
The Hyper-V PowerShell module was not found.

Enable Hyper-V first:
    Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All
and reboot.
'@
}
Write-Note 'Hyper-V module found'

# --- Download ---------------------------------------------------------------

Write-Step "Fetching the $Version release"

$api = if ($Version -eq 'latest') {
    "https://api.github.com/repos/$repo/releases/latest"
} else {
    "https://api.github.com/repos/$repo/releases/tags/$Version"
}

$release = Invoke-RestMethod -Uri $api -Headers @{ 'User-Agent' = 'hypervm-mcp-installer' }
$tag = $release.tag_name
Write-Note "version $tag"

$binaryUrl = ($release.assets | Where-Object { $_.name -eq 'hypervm-mcp.exe' }).browser_download_url
$sumUrl = ($release.assets | Where-Object { $_.name -eq 'hypervm-mcp.exe.sha256' }).browser_download_url
if (-not $binaryUrl) { throw "Release $tag has no hypervm-mcp.exe asset." }

New-Item -ItemType Directory -Force -Path $DownloadPath | Out-Null
$exe = Join-Path $DownloadPath 'hypervm-mcp.exe'
Invoke-WebRequest -Uri $binaryUrl -OutFile $exe -UseBasicParsing

# This binary is about to run as LocalSystem, so verify it rather than trust the
# transfer.
if ($sumUrl) {
    # Downloaded to a file rather than read from .Content: GitHub serves release
    # assets as application/octet-stream, so Invoke-WebRequest hands back a byte
    # array, and splitting that stringifies it into decimal byte values.
    $sumFile = Join-Path $DownloadPath 'hypervm-mcp.exe.sha256'
    Invoke-WebRequest -Uri $sumUrl -OutFile $sumFile -UseBasicParsing
    $expected = ((Get-Content $sumFile -Raw) -split '\s+')[0].Trim().ToLower()
    $actual = (Get-FileHash $exe -Algorithm SHA256).Hash.ToLower()
    if ($expected -ne $actual) {
        Remove-Item $exe -Force
        throw "Checksum mismatch. Expected $expected, got $actual. The download was not what the release published."
    }
    Write-Note "checksum verified: $actual"
} else {
    Write-Warning 'The release published no checksum; the binary could not be verified.'
}

# --- Install ----------------------------------------------------------------

Write-Step 'Installing the service'
Write-Note 'This raises one UAC prompt. Nothing after it will.'

& $exe service install
if ($LASTEXITCODE -ne 0) { throw "Service install failed with exit code $LASTEXITCODE." }

$installed = Join-Path $env:ProgramData 'hypervm-mcp\bin\hypervm-mcp.exe'
if (-not (Test-Path $installed)) { throw "Expected the service binary at $installed." }

Write-Step 'Checking the installation'
& $installed doctor
$doctor = $LASTEXITCODE

# --- Claude Code ------------------------------------------------------------

if (-not $SkipClaudeCode) {
    $claude = Get-Command claude -ErrorAction SilentlyContinue
    if ($claude) {
        Write-Step 'Registering with Claude Code'
        # -s user makes it available in every project rather than just this one.
        & claude mcp add hypervm-mcp -s user -- $installed bridge
        if ($LASTEXITCODE -eq 0) {
            Write-Note 'registered as "hypervm-mcp"'
        } else {
            Write-Warning 'Registration failed. Add it by hand with:'
            Write-Host "    claude mcp add hypervm-mcp -s user -- `"$installed`" bridge"
        }
    } else {
        Write-Note 'Claude Code was not found; skipping registration.'
        Write-Host "    Once installed, run: claude mcp add hypervm-mcp -s user -- `"$installed`" bridge"
    }
}

Write-Host ''
if ($doctor -eq 0) {
    Write-Host 'Done.' -ForegroundColor Green
} else {
    Write-Host 'Installed, but doctor reported problems above.' -ForegroundColor Yellow
}
Write-Host "Binary:  $installed"
Write-Host "Check:   hypervm-mcp doctor"
