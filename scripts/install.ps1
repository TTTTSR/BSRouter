<#
.SYNOPSIS
  BSRouter bsr installer - Windows.

.DESCRIPTION
  Usage:
    powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1 `
        [-Version <ver>] [-BaseUrl <url>] [-Prefix <dir>] [-Local -LocalDir <dir>] [-NoPath]

  -Version <ver>   release version to download (default: latest)
  -BaseUrl <url>   download base URL (default: https://github.com/TTTTSR/BSRouter/releases/download)
  -Prefix <dir>    install under <dir>\bin (default: %LOCALAPPDATA%\BSRouter)
  -Local -LocalDir <dir>  install from a local build directory instead of downloading
  -NoPath          do not modify the user PATH

  The downloaded asset is <BaseUrl>/<Version>/bsr-<Version>-windows-<arch>.zip and
  must contain gateway.exe, bsr.ps1 and bsr.cmd. With -Local, <dir> must hold the
  built gateway.exe and a scripts\bsr.ps1 + scripts\bsr.cmd.

.LICENSE
  MIT
#>
param(
    [string]$Version = 'latest',
    [string]$BaseUrl = 'https://github.com/TTTTSR/BSRouter/releases/download',
    [string]$Prefix = '',
    [switch]$Local,
    [string]$LocalDir = '',
    [switch]$NoPath
)

$ErrorActionPreference = 'Stop'

# Resolve-LatestVersion 把 -Version latest 解析为最新 release 的真实标签名
# (经 GitHub API releases/latest),再据此拼资产 URL——资产按版本命名
# (bsr-v0.2.0-windows-amd64.zip),不能直接按 "latest" 字面文件名下载。
function Resolve-LatestVersion {
    param([string]$BaseUrl)
    # BaseUrl 形如 https://host/OWNER/REPO/releases/download
    $parts = $BaseUrl.TrimEnd('/') -split '/'
    if ($parts.Count -lt 5) { throw "cannot derive repo from base url: $BaseUrl" }
    $api = "https://api.github.com/repos/$($parts[-4])/$($parts[-3])/releases/latest"
    $rel = Invoke-RestMethod -Uri $api -Headers @{ 'User-Agent' = 'bsr-installer' }
    if (-not $rel.tag_name) { throw "cannot resolve latest release from $api" }
    return [string]$rel.tag_name
}

if (-not $Prefix) { $Prefix = Join-Path $env:LOCALAPPDATA 'BSRouter' }
$BinDir = Join-Path $Prefix 'bin'
New-Item -ItemType Directory -Path $BinDir -Force | Out-Null

Write-Host '==> BSRouter bsr installer (Windows)'

if ($Local) {
    if (-not $LocalDir) { $LocalDir = (Get-Location).Path }
    Write-Host "==> Installing from local build: $LocalDir"
    $gatewaySrc = Join-Path $LocalDir 'gateway.exe'
    $bsrPs1Src  = Join-Path $LocalDir 'scripts\bsr.ps1'
    $bsrCmdSrc  = Join-Path $LocalDir 'scripts\bsr.cmd'
    if (-not (Test-Path -LiteralPath $gatewaySrc)) { throw "no gateway.exe in $LocalDir" }
    if (-not (Test-Path -LiteralPath $bsrPs1Src))  { throw "no scripts\bsr.ps1 in $LocalDir" }
    if (-not (Test-Path -LiteralPath $bsrCmdSrc))  { throw "no scripts\bsr.cmd in $LocalDir" }
    Copy-Item -LiteralPath $gatewaySrc -Destination (Join-Path $BinDir 'gateway.exe')
    Copy-Item -LiteralPath $bsrPs1Src  -Destination (Join-Path $BinDir 'bsr.ps1')
    Copy-Item -LiteralPath $bsrCmdSrc  -Destination (Join-Path $BinDir 'bsr.cmd')
} else {
    $arch = $env:PROCESSOR_ARCHITECTURE
    if ($arch -eq 'AMD64') { $arch = 'amd64' }
    elseif ($arch -eq 'ARM64') { $arch = 'arm64' }
    else { throw "unsupported architecture: $arch" }
    if ($Version -eq 'latest') {
        $Version = Resolve-LatestVersion -BaseUrl $BaseUrl
        Write-Host "==> Resolved latest release: $Version"
    }
    $asset = "bsr-$Version-windows-$arch.zip"
    $url = "$BaseUrl/$Version/$asset"
    Write-Host "==> Downloading $asset"
    Write-Host "    $url"
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("bsr-install-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Invoke-WebRequest -Uri $url -OutFile (Join-Path $tmp $asset) -UseBasicParsing
        Expand-Archive -LiteralPath (Join-Path $tmp $asset) -DestinationPath $tmp -Force
        Copy-Item -LiteralPath (Join-Path $tmp 'gateway.exe') -Destination (Join-Path $BinDir 'gateway.exe')
        Copy-Item -LiteralPath (Join-Path $tmp 'bsr.ps1')     -Destination (Join-Path $BinDir 'bsr.ps1')
        Copy-Item -LiteralPath (Join-Path $tmp 'bsr.cmd')     -Destination (Join-Path $BinDir 'bsr.cmd')
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Write-Host '==> Installed:'
Write-Host "    gateway.exe : $(Join-Path $BinDir 'gateway.exe')"
Write-Host "    bsr.cmd     : $(Join-Path $BinDir 'bsr.cmd')"

if (-not $NoPath) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }
    if ($userPath -notlike "*$BinDir*") {
        $newPath = if ($userPath) { "$userPath;$BinDir" } else { $BinDir }
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        Write-Host "==> Added $BinDir to the user PATH (effective in new terminals)."
    } else {
        Write-Host "==> $BinDir is already on the user PATH."
    }
} else {
    Write-Host "==> PATH modification skipped (-NoPath). Add $BinDir to PATH manually."
}

Write-Host '==> Done. Open a new terminal and run: bsr start'
