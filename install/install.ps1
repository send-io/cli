# send.io CLI installer for Windows (PowerShell 5.1+ or PowerShell 7+).
#
#   irm https://send.io/install.ps1 | iex
#   $env:SENDIO_VERSION = '1.2.3'; irm https://send.io/install.ps1 | iex
#   .\install.ps1 -Version 1.2.3
#
# Environment (same names as install.sh):
#   SENDIO_VERSION        version to install (default: latest release)
#   SENDIO_INSTALL        install root; binary goes to <root>\bin
#                         (default: $env:USERPROFILE\.sendio)
#   SENDIO_REPO           GitHub owner/name of the release repo (default: send-io/cli)
#   SENDIO_DOWNLOAD_BASE  base URL for release assets; files are fetched from
#                         <base>/v<version>/<file>
#   SENDIO_GITHUB_API     GitHub API base (default: https://api.github.com)
#   SENDIO_NO_MODIFY_PATH set to 1 to leave the user PATH alone
#
# Everything is inside Install-Sendio, invoked on the last line, so a
# truncated download does nothing.

function Install-Sendio {
    [CmdletBinding()]
    param(
        [string]$Version = $env:SENDIO_VERSION,
        [string]$InstallRoot = $env:SENDIO_INSTALL,
        [switch]$NoModifyPath,
        [switch]$Help
    )

    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with the progress bar on 5.1

    if (-not $env:SENDIO_NO_LOGO) {
        # The send.io mark, as the CLI shows it: a Signal Orange (#FF4A1C)
        # square with "S." cut out of it. Exact colour where the console
        # understands ANSI escapes (Windows Terminal, PowerShell 7), else the
        # nearest console colour. Built from char codes so the script stays ASCII.
        $e = [char]27
        $block = [string][char]0x2588
        $rows = @('##############', '###....#######', '###.##########', '###....#######', '######.#######', '###....##..###', '##############')
        $text = @('', '', 'send.io', 'CLI installer', '', '', '')
        Write-Host ''
        for ($r = 0; $r -lt $rows.Count; $r++) {
            $line = ' '
            foreach ($ch in $rows[$r].ToCharArray()) {
                if ($ch -eq '.') { $line += ' ' }
                elseif ($Host.UI.SupportsVirtualTerminal) { $line += "$e[38;2;255;74;28m$block$e[0m" }
                else { $line += $block }
            }
            if ($Host.UI.SupportsVirtualTerminal) {
                Write-Host ($line + "   $e[1m" + $text[$r] + "$e[0m")
            } else {
                Write-Host $line -ForegroundColor Red -NoNewline; Write-Host ('   ' + $text[$r])
            }
        }
        Write-Host ''
    }

    if ($Help) {
        Write-Output @'
send.io CLI installer (Windows)

Usage: install.ps1 [-Version <version>] [-InstallRoot <path>] [-NoModifyPath]

Defaults: latest release, installed to $env:USERPROFILE\.sendio\bin, which is
added to your user PATH. Env vars: SENDIO_VERSION, SENDIO_INSTALL, SENDIO_REPO,
SENDIO_DOWNLOAD_BASE, SENDIO_GITHUB_API, SENDIO_NO_MODIFY_PATH.
'@
        return
    }

    $repo = if ($env:SENDIO_REPO) { $env:SENDIO_REPO } else { 'send-io/cli' }
    # Each environment's web build fills these two in (frontend/console/scripts/gen-install-scripts.mjs).
    $defaultDownloadBase = ''
    $defaultLatestUrl = ''
    $downloadBase = if ($env:SENDIO_DOWNLOAD_BASE) { $env:SENDIO_DOWNLOAD_BASE } elseif ($defaultDownloadBase) { $defaultDownloadBase } else { "https://github.com/$repo/releases/download" }
    $latestUrl = if ($env:SENDIO_LATEST_URL) { $env:SENDIO_LATEST_URL } else { $defaultLatestUrl }
    $githubApi = if ($env:SENDIO_GITHUB_API) { $env:SENDIO_GITHUB_API } else { 'https://api.github.com' }
    if (-not $InstallRoot) {
        $home_ = if ($env:USERPROFILE) { $env:USERPROFILE } else { $HOME }
        $InstallRoot = Join-Path $home_ '.sendio'
    }
    if ($env:SENDIO_NO_MODIFY_PATH -eq '1') { $NoModifyPath = $true }

    # Older Windows PowerShell defaults to TLS 1.0.
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch { }

    # Architecture. PROCESSOR_ARCHITEW6432 is set when a 32-bit PowerShell
    # runs on a 64-bit OS.
    $procArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    if (-not $procArch) {
        try { $procArch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() } catch { }
    }
    switch -Regex ($procArch) {
        '^(AMD64|X64)$' { $arch = 'amd64'; break }
        '^ARM64$' { $arch = 'arm64'; break }
        default { throw "sendio-install: unsupported architecture '$procArch' (supported: amd64, arm64)" }
    }

    if (-not $Version -or $Version -eq 'latest') {
        if ($latestUrl) {
            Write-Host "sendio-install: resolving latest release..."
            $Version = ([string](Invoke-RestMethod -UseBasicParsing -Uri $latestUrl)).Trim()
        } else {
            Write-Host "sendio-install: resolving latest release of $repo..."
            $rel = Invoke-RestMethod -UseBasicParsing -Headers @{ Accept = 'application/vnd.github+json' } -Uri "$($githubApi.TrimEnd('/'))/repos/$repo/releases/latest"
            $Version = $rel.tag_name
        }
        if (-not $Version) { throw 'sendio-install: could not determine the latest version (set SENDIO_VERSION)' }
    }
    $Version = $Version -replace '^v', ''

    $archive = "sendio_${Version}_windows_${arch}.zip"
    $baseUrl = "$($downloadBase.TrimEnd('/'))/v$Version"

    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("sendio-" + [System.Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp -Force | Out-Null
    try {
        Write-Host "sendio-install: downloading sendio $Version (windows/$arch)..."
        $archivePath = Join-Path $tmp $archive
        $sumsPath = Join-Path $tmp 'checksums.txt'
        Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/$archive" -OutFile $archivePath
        Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/checksums.txt" -OutFile $sumsPath

        $expected = $null
        foreach ($line in Get-Content -LiteralPath $sumsPath) {
            $parts = $line.Trim() -split '\s+', 2
            if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq $archive) { $expected = $parts[0].ToLowerInvariant(); break }
        }
        if (-not $expected) { throw "sendio-install: no checksum for $archive in checksums.txt" }
        $actual = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected) {
            throw "sendio-install: checksum mismatch for $archive`n  expected: $expected`n  actual:   $actual`nrefusing to install"
        }
        Write-Host "sendio-install: checksum verified (sha256 $actual)"

        $extract = Join-Path $tmp 'x'
        Expand-Archive -LiteralPath $archivePath -DestinationPath $extract -Force
        $exe = Join-Path $extract 'sendio.exe'
        if (-not (Test-Path -LiteralPath $exe)) { throw "sendio-install: $archive does not contain sendio.exe" }

        $binDir = Join-Path $InstallRoot 'bin'
        New-Item -ItemType Directory -Path $binDir -Force | Out-Null
        $target = Join-Path $binDir 'sendio.exe'
        # A running sendio.exe cannot be overwritten but can be renamed.
        if (Test-Path -LiteralPath $target) {
            $old = "$target.old"
            Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
            Move-Item -LiteralPath $target -Destination $old -Force
        }
        Copy-Item -LiteralPath $exe -Destination $target -Force
        Remove-Item -LiteralPath "$target.old" -Force -ErrorAction SilentlyContinue
        Write-Host "sendio-install: installed sendio $Version to $target"
    }
    finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }

    # $IsWindows does not exist on Windows PowerShell 5.1 (which is Windows-only).
    $onWindows = ($PSVersionTable.PSVersion.Major -lt 6) -or $IsWindows
    $sep = [System.IO.Path]::PathSeparator
    $inSession = ($env:Path -split [regex]::Escape($sep)) -contains $binDir
    if (-not $NoModifyPath -and $onWindows) {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $entries = if ($userPath) { $userPath -split ';' } else { @() }
        if ($entries -notcontains $binDir) {
            $newPath = (@($entries | Where-Object { $_ }) + $binDir) -join ';'
            [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
            Write-Host "sendio-install: added $binDir to your user PATH (open a new terminal to pick it up)"
        }
    } elseif (-not $inSession) {
        Write-Host "sendio-install: add $binDir to your PATH to use sendio"
    }
    if (-not $inSession) { $env:Path = "$binDir$sep$env:Path" }

    Write-Host "sendio-install: run 'sendio login' to get started"
}

Install-Sendio @args
