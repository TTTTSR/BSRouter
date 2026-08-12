<#
.SYNOPSIS
  bsr - BSRouter gateway process manager (Windows / PowerShell 5.1+).

.DESCRIPTION
  Usage:
    bsr <command> [gateway args...]

  Commands:
    start    start the gateway in the background
    stop     stop the running gateway
    restart  stop + start (reuses last args unless new ones are given)
    status   show running state (exit 0 running, 3 stopped)
    run      run the gateway in the foreground (Ctrl+C exits)
    log      show the last 50 lines of the wrapper log; 'log tail' follows
    version  print version info
    help     this text

  Environment:
    BSR_GATEWAY     path to the gateway binary (overrides auto-detection)
    BSR_CONFIG_DIR  directory for bsr.pid / bsr.args / bsr.log

  Notes:
    - On Windows, `stop` uses Stop-Process (hard kill): a background process
      cannot receive a graceful SIGTERM, so the gateway's graceful shutdown
      does not run on Windows.
    - The gateway binary is located in this order: $env:BSR_GATEWAY ->
      same dir as this script -> parent dir -> PATH.
    - State files (config dir = %APPDATA%\BSRouter unless overridden):
      bsr.pid, bsr.args, bsr.stdout.log / bsr.stderr.log.

.LICENSE
  MIT
#>
param(
    [string]$Command = 'help',
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$GatewayArgs = @()
)

$BSR_SCRIPT_VERSION = '1.0.0'

function Write-Bsr { param([string]$Msg) Write-Host "bsr: $Msg" }
function Write-Err { param([string]$Msg) Write-Host "bsr: $Msg" -ForegroundColor Red }

# --- resolve script dir -----------------------------------------------------
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path

# --- locate the gateway binary ----------------------------------------------
function Find-Gateway {
    if ($env:BSR_GATEWAY) { return $env:BSR_GATEWAY }
    $parent = Split-Path -Parent $ScriptDir
    foreach ($c in @(
        (Join-Path $ScriptDir 'gateway.exe'),
        (Join-Path $ScriptDir 'gateway'),
        (Join-Path $parent 'gateway.exe'),
        (Join-Path $parent 'gateway')
    )) {
        if (Test-Path -LiteralPath $c) { return $c }
    }
    $cmd = Get-Command 'gateway.exe' -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    return $null
}

$Gateway = Find-Gateway
if (-not $Gateway) {
    Write-Err "cannot find the gateway binary ('gateway.exe'). Build it with 'go build ./cmd/gateway', set BSR_GATEWAY, or run the installer."
    exit 1
}

# --- resolve config dir ------------------------------------------------------
if ($env:BSR_CONFIG_DIR) {
    $ConfigDir = $env:BSR_CONFIG_DIR
} else {
    $ConfigDir = Join-Path $env:APPDATA 'BSRouter'
}
New-Item -ItemType Directory -Path $ConfigDir -Force | Out-Null

$PidFile   = Join-Path $ConfigDir 'bsr.pid'
$ArgsFile  = Join-Path $ConfigDir 'bsr.args'
$StdoutLog = Join-Path $ConfigDir 'bsr.stdout.log'
$StderrLog = Join-Path $ConfigDir 'bsr.stderr.log'

# --- helpers -----------------------------------------------------------------
function Read-Pid {
    if (Test-Path -LiteralPath $PidFile) {
        $t = (Get-Content -LiteralPath $PidFile -Raw -ErrorAction SilentlyContinue).Trim()
        if ($t -match '^\d+$') { return [int]$t }
    }
    return 0
}

function Test-PidAlive {
    param([int]$ProcPid)
    if ($ProcPid -le 0) { return $false }
    try { Get-Process -Id $ProcPid -ErrorAction Stop | Out-Null; return $true }
    catch { return $false }
}

function Save-Args {
    param([string[]]$GArgs)
    if ($GArgs -and $GArgs.Count -gt 0) {
        ($GArgs | ConvertTo-Json -Compress) | Set-Content -LiteralPath $ArgsFile -Encoding UTF8
    } else {
        Remove-Item -LiteralPath $ArgsFile -Force -ErrorAction SilentlyContinue
    }
}

function Read-SavedArgs {
    if (Test-Path -LiteralPath $ArgsFile) {
        try {
            $j = Get-Content -LiteralPath $ArgsFile -Raw -ErrorAction Stop
            $a = $j | ConvertFrom-Json
            if ($a -is [System.Array]) { return [string[]]$a }
            if ($a) { return @([string]$a) }
        } catch { }
    }
    return @()
}

function Show-Help {
    Write-Host @"
bsr - BSRouter gateway process manager v$BSR_SCRIPT_VERSION
gateway: $Gateway
config : $ConfigDir

Usage: bsr <command> [gateway args...]

Commands:
  start    start the gateway in the background
  stop     stop the running gateway
  restart  stop then start (reuses last args unless new ones are given)
  status   show running state (exit 0 running, 3 stopped)
  run      run the gateway in the foreground
  log      show the last 50 lines of the wrapper log; 'log tail' follows
  version  print version info
  help     this text

Examples:
  bsr start -private
  bsr start -addr :9000 -api-key sk-...
  bsr restart
  bsr log tail
"@
}

# --- commands -----------------------------------------------------------------
function Start-Bsr {
    param([string[]]$GArgs)
    $procPid = Read-Pid
    if (Test-PidAlive -ProcPid $procPid) {
        Write-Bsr "already running (pid $procPid). Stop it first or use 'restart'."
        return 0
    }
    if (-not $GArgs -or $GArgs.Count -eq 0) {
        $GArgs = Read-SavedArgs
    }
    Save-Args -GArgs $GArgs

    $p = Start-Process -FilePath $Gateway -ArgumentList $GArgs -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $StdoutLog -RedirectStandardError $StderrLog
    $newPid = $p.Id
    Set-Content -LiteralPath $PidFile -Value $newPid -Encoding UTF8

    # wait up to 3s for the process to appear and survive
    $deadline = (Get-Date).AddSeconds(3)
    while ((Get-Date) -lt $deadline) {
        if (-not (Test-PidAlive -ProcPid $newPid)) { break }
        Start-Sleep -Milliseconds 100
    }
    if (-not (Test-PidAlive -ProcPid $newPid)) {
        Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue
        Write-Err "gateway failed to start. Last log lines:"
        Get-Content -LiteralPath $StderrLog -Tail 15 -ErrorAction SilentlyContinue | ForEach-Object { Write-Host "  $_" }
        return 1
    }
    # settle, then require it is still alive (catches bind/startup failures)
    Start-Sleep -Seconds 1
    if (-not (Test-PidAlive -ProcPid $newPid)) {
        Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue
        Write-Err "gateway exited shortly after start. Last log lines:"
        Get-Content -LiteralPath $StderrLog -Tail 15 -ErrorAction SilentlyContinue | ForEach-Object { Write-Host "  $_" }
        return 1
    }

    Write-Bsr "started pid $newPid"
    Write-Bsr "wrapper log: $StderrLog (gateway stderr; stdout: $StdoutLog)"
    return 0
}

function Stop-Bsr {
    $procPid = Read-Pid
    if (-not (Test-PidAlive -ProcPid $procPid)) {
        Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue
        Write-Bsr "not running."
        return 0
    }
    Write-Bsr "stopping pid $procPid ..."
    Stop-Process -Id $procPid -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue
    Write-Bsr "stopped."
    return 0
}

function Restart-Bsr {
    param([string[]]$GArgs)
    Stop-Bsr | Out-Null
    return (Start-Bsr -GArgs $GArgs)
}

function Show-Status {
    $procPid = Read-Pid
    if (Test-PidAlive -ProcPid $procPid) {
        Write-Bsr "running (pid $procPid, log: $StderrLog)"
        return 0
    }
    Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue
    Write-Bsr "not running."
    return 3
}

function Run-Bsr {
    param([string[]]$GArgs)
    & $Gateway @GArgs
    exit $LASTEXITCODE
}

function Show-Log {
    param([string]$Follow)
    # the gateway's own log output goes to stderr (Go's log package), so that
    # file is the primary wrapper log; fall back to stdout if it is missing.
    $log = $StderrLog
    if (-not (Test-Path -LiteralPath $log)) { $log = $StdoutLog }
    if (-not (Test-Path -LiteralPath $log)) {
        Write-Err "no wrapper log yet (start the gateway first): $StdoutLog"
        return 1
    }
    if ($Follow -eq 'tail' -or $Follow -eq '-f') {
        Get-Content -LiteralPath $log -Tail 50 -Wait
    } else {
        Get-Content -LiteralPath $log -Tail 50
    }
}

function Show-Version {
    Write-Host "bsr $BSR_SCRIPT_VERSION"
    Write-Host "gateway: $Gateway"
    $out = & $Gateway -version 2>&1
    if ($LASTEXITCODE -eq 0) { $out } else { Write-Host "(gateway does not support -version)" }
}

# --- dispatch -----------------------------------------------------------------
switch ($Command.ToLowerInvariant()) {
    'start'   { exit (Start-Bsr -GArgs $GatewayArgs) }
    'stop'    { exit (Stop-Bsr) }
    'restart' { exit (Restart-Bsr -GArgs $GatewayArgs) }
    'status'  { exit (Show-Status) }
    'run'     { Run-Bsr -GArgs $GatewayArgs }
    'log'     { Show-Log -Follow ($GatewayArgs -join ' ') }
    'version' { Show-Version }
    'help'    { Show-Help }
    default   { Write-Err "unknown command '$Command'."; Show-Help; exit 2 }
}
