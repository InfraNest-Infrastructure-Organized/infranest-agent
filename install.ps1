<#
.SYNOPSIS
    Install the InfraNest monitoring agent on Windows.

.DESCRIPTION
    Downloads a versioned binary, verifies its checksum, and registers a scheduled task that runs it at
    boot. PowerShell does the installing; the agent itself is a single Go binary with no dependencies and
    no PowerShell involved once it is running.

    The agent only sends. It takes no instructions, runs no commands, and opens no ports.

.EXAMPLE
    .\install.ps1 -Token sat_xxxxx

.EXAMPLE
    .\install.ps1 -Uninstall
#>
[CmdletBinding()]
param(
    # The server token, from your server's page in InfraNest.
    [string]$Token,

    # Read the token from a file instead, so it never reaches your PowerShell history.
    [string]$TokenFile,

    # Where to send readings.
    [string]$Url = 'https://ingest.infranest.app',

    # Install a specific version instead of the latest.
    [string]$Version = 'latest',

    # Install a binary you already have, instead of downloading one.
    [string]$From,

    # Remove the agent, its task, its config and its data.
    [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'

$Repo        = 'InfraNest-Infrastructure-Organized/infranest-agent'
$InstallDir  = Join-Path $env:ProgramFiles 'InfraNest'
$ConfDir     = Join-Path $env:ProgramData 'InfraNest'
$BinPath     = Join-Path $InstallDir 'infranest-agent.exe'
$ConfPath    = Join-Path $ConfDir 'agent.conf'
$TaskName    = 'InfraNest agent'

function Write-Step { param($m) Write-Host "  $m" }
function Write-Warn { param($m) Write-Warning $m }

# Creating a service and writing under Program Files both need it, and refusing early is kinder than
# failing halfway through.
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'This needs an elevated PowerShell. Right-click PowerShell and choose "Run as administrator".'
}

# ── Uninstall ────────────────────────────────────────────────────────────────────────────────────────
if ($Uninstall) {
    Write-Host "`nRemoving the InfraNest agent.`n"

    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask   -TaskName $TaskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
        Write-Step 'removed the scheduled task'
    }

    Get-Process -Name 'infranest-agent' -ErrorAction SilentlyContinue | Stop-Process -Force

    foreach ($path in @($InstallDir, $ConfDir)) {
        if (Test-Path $path) {
            Remove-Item $path -Recurse -Force
            Write-Step "removed $path"
        }
    }

    Write-Host "`nDone. Nothing of the agent is left on this machine.`n"
    return
}

# ── The token ────────────────────────────────────────────────────────────────────────────────────────
if ($TokenFile) {
    if (-not (Test-Path $TokenFile)) { throw "Cannot read the token file: $TokenFile" }
    $Token = (Get-Content $TokenFile -Raw).Trim()
}
if (-not $Token) {
    throw 'A token is required. Get one from your server''s page in InfraNest, then: .\install.ps1 -Token sat_xxxxx'
}

# ── Which build ──────────────────────────────────────────────────────────────────────────────────────
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

Write-Host "`nInstalling the InfraNest agent (windows/$arch).`n"

$tmp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    $staged = Join-Path $tmp 'infranest-agent.exe'

    if ($From) {
        # Installing a binary you built yourself. Nothing to verify — you made it.
        if (-not (Test-Path $From)) { throw "Cannot read $From" }
        Copy-Item $From $staged
        Write-Step "using $From"
    }
    else {
        $base = if ($Version -eq 'latest') {
            "https://github.com/$Repo/releases/latest/download"
        } else {
            "https://github.com/$Repo/releases/download/$Version"
        }
        $name = "infranest-agent_windows_$arch.exe"

        Write-Step "downloading $name"
        # TLS 1.2 explicitly: Windows PowerShell 5.1 still defaults to older protocols on some builds, and
        # the download simply fails with a confusing error rather than saying why.
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri "$base/$name" -OutFile $staged -UseBasicParsing

        Write-Step 'verifying the checksum'
        $sumFile = Join-Path $tmp 'sha256'
        Invoke-WebRequest -Uri "$base/$name.sha256" -OutFile $sumFile -UseBasicParsing

        $expected = ((Get-Content $sumFile -Raw).Trim() -split '\s+')[0]
        $actual   = (Get-FileHash $staged -Algorithm SHA256).Hash.ToLower()
        if ($expected.ToLower() -ne $actual) {
            throw "Checksum mismatch. Not installing. Expected $expected, got $actual"
        }
    }

    # ── Files ────────────────────────────────────────────────────────────────────────────────────────
    New-Item -ItemType Directory -Path $InstallDir, $ConfDir -Force | Out-Null
    Copy-Item $staged $BinPath -Force
    Write-Step "installed to $BinPath"

    "INFRANEST_TOKEN=$Token`r`nINFRANEST_URL=$Url`r`n" |
        Set-Content -Path $ConfPath -Encoding ASCII -NoNewline

    # The config holds a credential, so only Administrators and SYSTEM may read it. Inheritance is
    # disabled first, or the permissive defaults on ProgramData survive everything set afterwards.
    $acl = Get-Acl $ConfPath
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($who in 'BUILTIN\Administrators', 'NT AUTHORITY\SYSTEM') {
        $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule(
            $who, 'FullControl', 'Allow')))
    }
    Set-Acl -Path $ConfPath -AclObject $acl
    Write-Step "wrote $ConfPath (Administrators and SYSTEM only)"

    # ── The task ─────────────────────────────────────────────────────────────────────────────────────
    #
    # A scheduled task at boot, running one long-lived process — not a repeating task. The Task Scheduler
    # cannot repeat faster than once a minute, and the agent samples every 20 seconds, so the timing has
    # to live inside the process. It also avoids a Windows service, which would mean depending on
    # golang.org/x/sys/windows/svc and giving up the agent's zero-dependency guarantee for one platform.
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    }

    $action    = New-ScheduledTaskAction  -Execute $BinPath -Argument 'run'
    $trigger   = New-ScheduledTaskTrigger -AtStartup
    $principal = New-ScheduledTaskPrincipal -UserId 'NT AUTHORITY\LOCAL SERVICE' -LogonType ServiceAccount -RunLevel Limited
    $settings  = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
                    -StartWhenAvailable -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `
                    -ExecutionTimeLimit ([TimeSpan]::Zero)

    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
        -Principal $principal -Settings $settings `
        -Description 'Collects CPU, memory, disk and load and sends them to InfraNest. It only sends.' | Out-Null

    # LOCAL SERVICE, not SYSTEM: it is the least-privileged account that still has network access, and
    # nothing the agent reads needs more than that.
    $acl = Get-Acl $ConfPath
    $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule(
        'NT AUTHORITY\LOCAL SERVICE', 'Read', 'Allow')))
    Set-Acl -Path $ConfPath -AclObject $acl

    Start-ScheduledTask -TaskName $TaskName
    Write-Step 'scheduled task registered and started'
}
finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host @"

Done.

See exactly what this machine will send, right now:
    & '$BinPath' print

Check it is working:
    & '$BinPath' status

Remove it completely:
    .\install.ps1 -Uninstall

"@
