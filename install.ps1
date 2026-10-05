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
    .\install.ps1 -Upgrade

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
    [string]$Url = 'https://ingest.infranest.io',

    # Install a specific version instead of the latest.
    [string]$Version = 'latest',

    # Install a binary you already have, instead of downloading one.
    [string]$From,

    # Also report the busiest processes by name. Program names only — never their arguments, which
    # routinely carry credentials and are a separate setting for exactly that reason.
    [switch]$Processes,

    # Replace the binary of an agent already installed here. Needs no token, and leaves its
    # configuration exactly as it is.
    [switch]$Upgrade,

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

# ── Where releases come from ─────────────────────────────────────────────────────────────────────────
# Our download host first, GitHub second — the same order and the same reasons as install.sh: GitHub has
# no IPv6 address, get.infranest.io passes the very same release assets through over both families, and
# some networks allow only github.com. Both serve `/releases/download/<tag>/<file>` from one release.
$script:Sources = @('https://get.infranest.io', "https://github.com/$Repo")

# Try this source first from now on, so a source that just timed out is not waited on again per file.
function Use-Source { param($src) $script:Sources = @($src) + @($script:Sources | Where-Object { $_ -ne $src }) }

# Pin `latest` to one tag before downloading anything. The binary and its checksum are two requests, and
# two `latest` requests either side of a release would fail the checksum in a way that reads as tampering.
function Resolve-Version {
    if ($Version -ne 'latest') { return $Version }
    $tag = ''
    try {
        $tag = [string](Invoke-WebRequest -Uri 'https://get.infranest.io/releases/latest' -UseBasicParsing -TimeoutSec 30).Content
        $tag = $tag.Trim()
    } catch { $tag = '' }
    if ($tag -notmatch '^v\d+\.\d+\.\d+') {
        # GitHub answers `releases/latest` with a redirect to `.../releases/tag/<tag>`. Read it without
        # following it — HttpWebRequest because Invoke-WebRequest treats an unfollowed redirect as an
        # error on 5.1 and 7 in different ways.
        try {
            $req = [Net.HttpWebRequest]::Create("https://github.com/$Repo/releases/latest")
            $req.AllowAutoRedirect = $false
            $req.Timeout = 30000
            $res = $req.GetResponse()
            $tag = ([string]$res.Headers['Location'] -split '/releases/tag/')[-1]
            $res.Close()
        } catch { $tag = '' }
        if ($tag -match '^v\d+\.\d+\.\d+') { Use-Source "https://github.com/$Repo" }
    }
    if ($tag -notmatch '^v\d+\.\d+\.\d+') {
        throw 'Could not find the latest release: neither get.infranest.io nor github.com answered.'
    }
    return $tag
}

# One release asset, from the first source that can serve it. Falls through ONLY on a failed download;
# a checksum mismatch afterwards stops the install and is never retried elsewhere.
function Save-Asset {
    param($file, $dest)
    foreach ($src in $script:Sources) {
        try {
            Invoke-WebRequest -Uri "$src/releases/download/$Version/$file" -OutFile $dest -UseBasicParsing -TimeoutSec 300
            Use-Source $src
            return
        } catch {
            Write-Warn "could not download $file from $($src -replace '/InfraNest-.*$', ''), trying the next source"
        }
    }
    throw "Download failed. Check that release $Version exists: https://github.com/$Repo/releases"
}

# ── Who built it ─────────────────────────────────────────────────────────────────────────────────────
# The same rule as install.sh, for the same reasons: the checksum comes from the same place as the binary,
# so the build attestation is what says who built it. Checked when gh or cosign is already installed,
# pinned to release.yml. Verification failing stops the install; being unable to check says so and goes on.
$Workflow    = "$Repo/.github/workflows/release.yml"
$IdentityRe  = "^https://github\.com/$Repo/\.github/workflows/release\.yml@refs/tags/v"
$Attestation = 'infranest-agent.sigstore.json'

# A native command's stderr is not its verdict; its exit code is. Windows PowerShell 5.1 turns every line
# a native command writes to stderr into an error record, and under $ErrorActionPreference = 'Stop' the
# first one ends the script. So cosign's deprecation notice, or gh saying it is not logged in, stopped the
# install before the exit code was ever read — a genuine release could not be installed with cosign on
# 5.1, and a refusal read as a crash instead of saying why (InfraNest#2375). Scoped to this function, so
# everything else keeps stopping on the first error.
#
# The exit code starts at a failure, so a command that never ran cannot leave an earlier success behind.
function Invoke-Native {
    param([string]$exe, [string[]]$argv)
    $ErrorActionPreference = 'Continue'
    $global:LASTEXITCODE = 1
    $script:NativeOut = @(& $exe @argv 2>&1 | ForEach-Object { "$_" })
    return $LASTEXITCODE
}

function Test-Provenance {
    param($bin)
    $bundle = Join-Path $tmp 'attestation.json'
    $haveBundle = $false
    if (-not $From) {
        try { Save-Asset $Attestation $bundle 3>$null; $haveBundle = $true } catch { }
    }

    $gh = Get-Command gh -ErrorAction SilentlyContinue
    $cosign = Get-Command cosign -ErrorAction SilentlyContinue
    $ghAuthed = $false
    if ($gh -and -not $haveBundle) { $ghAuthed = ((Invoke-Native 'gh' @('auth', 'status')) -eq 0) }

    if ($gh -and ($haveBundle -or $ghAuthed)) {
        $tool = 'gh'
        $argv = @('attestation', 'verify', $bin, '--repo', $Repo, '--signer-workflow', $Workflow)
        if ($haveBundle) { $argv += @('--bundle', $bundle) }
    } elseif ($cosign -and $haveBundle) {
        $tool = 'cosign'
        $argv = @('verify-blob-attestation', '--bundle', $bundle, '--new-bundle-format', '--type', 'slsaprovenance1',
                  '--certificate-oidc-issuer', 'https://token.actions.githubusercontent.com',
                  '--certificate-identity-regexp', $IdentityRe, $bin)
    } else {
        if (-not $gh -and -not $cosign) {
            Write-Step 'build provenance not checked: neither gh nor cosign is installed (the checksum was)'
        } elseif ($From) {
            Write-Step 'build provenance not checked: no attestation to look up for a -From binary without a logged-in gh'
        } else {
            Write-Warn "build provenance not checked: release $Version has no attestation file to verify against"
        }
        return
    }

    Write-Step "verifying who built it ($tool)"
    if ((Invoke-Native $tool $argv) -ne 0) {
        $script:NativeOut | ForEach-Object { Write-Host "    $_" }
        throw "The build attestation does not verify. Not installing. This binary was not built by $Workflow."
    }
    Write-Step "built by $Workflow"
}

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
if ($Upgrade) {
    # An upgrade changes the binary and keeps every decision already made on this machine. So there has
    # to be an installation to keep, and the options that would make one of those decisions again are
    # refused rather than ignored: a switch that is accepted and does nothing reads as one that worked.
    if (-not (Test-Path $ConfPath)) {
        throw "There is no agent to upgrade here (no $ConfPath). Install it with -Token instead."
    }
    if ($Token -or $TokenFile) {
        throw '-Upgrade keeps the token this machine already has. Run it without -Token.'
    }
    if ($Processes -or $PSBoundParameters.ContainsKey('Url')) {
        throw "-Upgrade leaves the configuration alone. To change a setting, edit $ConfPath and restart the task."
    }
}
else {
    if ($TokenFile) {
        if (-not (Test-Path $TokenFile)) { throw "Cannot read the token file: $TokenFile" }
        $Token = (Get-Content $TokenFile -Raw).Trim()
    }
    if (-not $Token) {
        throw 'A token is required. Get one from your server''s page in InfraNest, then: .\install.ps1 -Token sat_xxxxx'
    }
}

# ── Which build ──────────────────────────────────────────────────────────────────────────────────────
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

$doing = if ($Upgrade) { 'Upgrading' } else { 'Installing' }
Write-Host "`n$doing the InfraNest agent (windows/$arch).`n"

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
        # TLS 1.2 explicitly: Windows PowerShell 5.1 still defaults to older protocols on some builds, and
        # the download simply fails with a confusing error rather than saying why.
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

        $Version = Resolve-Version
        Write-Step "release $Version"
        $name = "infranest-agent_windows_$arch.exe"

        Write-Step "downloading $name"
        Save-Asset $name $staged

        Write-Step 'verifying the checksum'
        $sumFile = Join-Path $tmp 'sha256'
        Save-Asset "$name.sha256" $sumFile

        $expected = ((Get-Content $sumFile -Raw).Trim() -split '\s+')[0]
        $actual   = (Get-FileHash $staged -Algorithm SHA256).Hash.ToLower()
        if ($expected.ToLower() -ne $actual) {
            throw "Checksum mismatch. Not installing. Expected $expected, got $actual"
        }
    }

    Test-Provenance $staged

    # ── Files ────────────────────────────────────────────────────────────────────────────────────────
    New-Item -ItemType Directory -Path $InstallDir, $ConfDir -Force | Out-Null

    # Stopped before the copy, not after. Windows will not replace an executable that is running, so on a
    # machine that already has the agent the copy below fails with "being used by another process" —
    # which is every upgrade, and every re-install.
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    }
    Get-Process -Name 'infranest-agent' -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Seconds 1

    Copy-Item $staged $BinPath -Force
    Write-Step "installed to $BinPath"

    if ($Upgrade) {
        Write-Step "keeping $ConfPath as it is"
    }
    else {
        # Written at install time because the agent reads its configuration once, at startup: turning this
        # on afterwards means editing this file and restarting the task, and install is the only moment it
        # costs a switch.
        $conf = "INFRANEST_TOKEN=$Token`r`nINFRANEST_URL=$Url`r`n"
        if ($Processes) { $conf += "INFRANEST_PROCESSES=1`r`n" }
        $conf | Set-Content -Path $ConfPath -Encoding ASCII -NoNewline

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
    }

    # ── The task ─────────────────────────────────────────────────────────────────────────────────────
    #
    # A scheduled task at boot, running one long-lived process — not a repeating task. The Task Scheduler
    # cannot repeat faster than once a minute, and the agent samples every 20 seconds, so the timing has
    # to live inside the process. It also avoids a Windows service, which would mean depending on
    # golang.org/x/sys/windows/svc and giving up the agent's zero-dependency guarantee for one platform.
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    }

    # A scheduled task inherits no environment — no EnvironmentFile, nothing. That is why the agent
    # reads the config file itself (internal/config/file.go), and why the path is named here rather
    # than left to a default: relying on PROGRAMDATA being set inside a LOCAL SERVICE task is an
    # assumption, and the symptom if it is ever wrong is an agent that starts and sends nowhere.
    $action    = New-ScheduledTaskAction -Execute $BinPath -Argument "run --config `"$ConfPath`""
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

    # Started, then checked a moment later. Nothing here runs `run` to find out whether it works: `run`
    # is a long-lived loop, so a probe that waits for it to exit waits for ever and the installer hangs
    # on its last step with no output. An earlier build did exactly that deliberately, because back then
    # `run` returned an error immediately; the check outlived the reason for it.
    #
    # `status` is the safe probe: it reads what is on disk and returns.
    Start-ScheduledTask -TaskName $TaskName
    Start-Sleep -Seconds 3

    $state = (Get-ScheduledTask -TaskName $TaskName).State
    if ($state -eq 'Running') {
        Write-Step 'scheduled task registered and started'
    } else {
        Write-Warn "the task did not stay running (state: $state). What the agent says:"
        & $BinPath status --config $ConfPath 2>&1 | ForEach-Object { Write-Host "    $_" }
    }
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
