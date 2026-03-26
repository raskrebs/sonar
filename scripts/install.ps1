# Sonar installer for Windows
# Usage: irm https://raw.githubusercontent.com/raskrebs/sonar/main/scripts/install.ps1 | iex

$ErrorActionPreference = "Stop"

# TLS 1.2 required for GitHub API (PS 5.1 defaults to TLS 1.0)
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repo = "raskrebs/sonar"
if ($env:SONAR_INSTALL_DIR) {
    $InstallDir = $env:SONAR_INSTALL_DIR
} else {
    $InstallDir = Join-Path $env:LOCALAPPDATA "sonar"
}

function Write-Info($msg) { Write-Host "sonar " -ForegroundColor Cyan -NoNewline; Write-Host $msg }
function Write-Ok($msg) { Write-Host "  OK " -ForegroundColor Green -NoNewline; Write-Host $msg }
function Write-Err($msg) { Write-Host "  ERR " -ForegroundColor Red -NoNewline; Write-Host $msg; exit 1 }

# Detect architecture ($env:PROCESSOR_ARCHITECTURE works on PowerShell 5.x;
# RuntimeInformation is unavailable there and crashes the script)
$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { Write-Err "Unsupported architecture: $_" }
}

$Platform = "windows_$Arch"
Write-Info "Detected platform: $Platform"

# Fetch release
if ($env:SONAR_VERSION) {
    Write-Info "Fetching release $($env:SONAR_VERSION)..."
    $ReleaseUrl = "https://api.github.com/repos/$Repo/releases/tags/$($env:SONAR_VERSION)"
} else {
    Write-Info "Fetching latest release..."
    $ReleaseUrl = "https://api.github.com/repos/$Repo/releases/latest"
}
try {
    $Release = Invoke-RestMethod $ReleaseUrl
} catch {
    Write-Err "Failed to fetch release. Check https://github.com/$Repo/releases"
}

# Find the right asset
$Asset = $Release.assets | Where-Object { $_.name -like "*$Platform*" -and $_.name -like "*.zip" } | Select-Object -First 1
if (-not $Asset) {
    Write-Err "No binary found for $Platform in release $($Release.tag_name)"
}

$Tag = $Release.tag_name
Write-Info "Downloading sonar $Tag for $Platform..."

# Download and extract
$TmpDir = Join-Path ([System.IO.Path]::GetTempPath()) "sonar-install-$(Get-Random)"
New-Item -ItemType Directory -Path $TmpDir -Force | Out-Null

try {
    $ZipPath = Join-Path $TmpDir "sonar.zip"
    Invoke-WebRequest -Uri $Asset.browser_download_url -OutFile $ZipPath -UseBasicParsing

    Expand-Archive -Path $ZipPath -DestinationPath $TmpDir -Force

    # Install
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null

    $Binary = Get-ChildItem -Path $TmpDir -Filter "sonar.exe" -Recurse | Select-Object -First 1
    if (-not $Binary) {
        Write-Err "sonar.exe not found in release archive"
    }

    Copy-Item $Binary.FullName (Join-Path $InstallDir "sonar.exe") -Force
    Write-Ok "Installed sonar $Tag to $InstallDir\sonar.exe"
} finally {
    Remove-Item -Recurse -Force $TmpDir -ErrorAction SilentlyContinue
}

# Add to PATH if needed
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($UserPath -split ";" | Where-Object { $_ -eq $InstallDir }) {
    Write-Host "  sonar is already in PATH" -ForegroundColor DarkGray
} else {
    [Environment]::SetEnvironmentVariable("Path", "$UserPath;$InstallDir", "User")
    Write-Ok "Added $InstallDir to user PATH"
    Write-Host ""
    Write-Info "Restart your terminal for PATH changes to take effect."
}

Write-Host ""
Write-Ok "Done! Run 'sonar' to get started."
