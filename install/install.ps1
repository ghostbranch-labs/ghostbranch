# Install Ghostbranch on Windows (PowerShell 5.1 or later):
#
#   irm https://ghostbranch.site/install.ps1 | iex
#
# It downloads the release archive and the release's checksums.txt from
# GitHub over HTTPS, refuses the archive unless its SHA-256 matches, puts
# ghostbranch.exe in %LOCALAPPDATA%\Programs\ghostbranch and adds that folder
# to your user PATH. It needs no admin rights.
# A copy installed elsewhere is upgraded by running this again with GHOSTBRANCH_INSTALL_DIR set to its folder.
# Source: https://github.com/ghostbranch-labs/ghostbranch/blob/main/install/install.ps1
#
# Settings (environment variables):
#   GHOSTBRANCH_VERSION      a release tag to install, such as v0.1.0 (default: the latest release)
#   GHOSTBRANCH_INSTALL_DIR  where to put the binary (default: %LOCALAPPDATA%\Programs\ghostbranch)
#   GHOSTBRANCH_ARCH         amd64 installs the amd64 build on Windows on ARM, which runs it under emulation
# For tests only:
#   GHOSTBRANCH_DOWNLOAD_BASE  replaces https://github.com/ghostbranch-labs/ghostbranch/releases;
#                              must be https://, or http://127.0.0.1 or http://localhost
#   GHOSTBRANCH_TEST_ARCH      pretends the processor is this (AMD64, ARM64, X86)
#   GHOSTBRANCH_TEST_SKIP_PATH set to 1: report the user PATH change instead of making it
#
# Errors are thrown, not exit codes, so a failure under iex doesn't close the
# window. Everything runs inside one function, called on the last line, so a
# download cut short runs nothing.

function Install-Ghostbranch {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is far slower with the progress bar on 5.1
    $supported = 'windows_amd64 (Windows on ARM: set GHOSTBRANCH_ARCH=amd64 to run it under emulation)'

    if ((Test-Path variable:IsWindows) -and -not $IsWindows) {
        throw 'ghostbranch install: install.ps1 is for Windows; on macOS and Linux use install.sh.'
    }

    $machine = $env:GHOSTBRANCH_TEST_ARCH
    if (-not $machine) {
        try {
            $machine = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
        } catch {
            $machine = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
        }
    }
    switch ($machine.ToUpperInvariant()) {
        { $_ -in 'X64', 'AMD64' } { $arch = 'amd64' }
        'ARM64' {
            if ($env:GHOSTBRANCH_ARCH -ne 'amd64') {
                throw "ghostbranch install: there is no Windows on ARM build. Windows on ARM runs the amd64 build under emulation; to install it, set `$env:GHOSTBRANCH_ARCH = 'amd64' and run this again. Supported: $supported"
            }
            $arch = 'amd64'
        }
        default { throw "ghostbranch install: no build for Windows $machine. Supported: $supported" }
    }

    function Assert-Version([string]$v, [string]$what) {
        if ($v -cnotmatch '^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$') {
            throw "ghostbranch install: $what isn't a version (such as 0.1.0 or v0.1.0): $v"
        }
    }
    $pin = if ($env:GHOSTBRANCH_VERSION) { $env:GHOSTBRANCH_VERSION.TrimStart('v') } else { '' }
    if ($pin) { Assert-Version $pin 'GHOSTBRANCH_VERSION' }

    $base = $env:GHOSTBRANCH_DOWNLOAD_BASE
    if (-not $base) { $base = 'https://github.com/ghostbranch-labs/ghostbranch/releases' }
    $base = $base.TrimEnd('/')
    if ($base.Contains('@') -or -not ($base -match '^https://' -or $base -match '^http://(127\.0\.0\.1|localhost)(:[0-9]+)?(/|$)')) {
        throw "ghostbranch install: GHOSTBRANCH_DOWNLOAD_BASE must be https://, or http://127.0.0.1 or http://localhost: $base"
    }
    $local = $base.StartsWith('http://')

    $dir = $env:GHOSTBRANCH_INSTALL_DIR
    if (-not $dir) { $dir = Join-Path $env:LOCALAPPDATA 'Programs\ghostbranch' }

    try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch { }
    $new = $null
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('ghostbranch-install-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        function Get-File([string]$url, [string]$file) {
            try {
                # A redirect must stay on https (or on the local test server).
                $r = Invoke-WebRequest -Uri $url -OutFile $file -UseBasicParsing -PassThru -MaximumRedirection 5
                $final = $r.BaseResponse.ResponseUri
                if ($null -eq $final) { $final = $r.BaseResponse.RequestMessage.RequestUri }
                if ($null -ne $final -and $final.Scheme -ne 'https' -and -not $local) {
                    throw "redirected to $final"
                }
            } catch {
                throw "ghostbranch install: couldn't download ${url}: $($_.Exception.Message)"
            }
        }

        if ($pin) {
            $from = "$base/download/v$pin"
        } else {
            $from = "$base/latest/download"
        }
        $sums = Join-Path $tmp 'checksums.txt'
        Get-File "$from/checksums.txt" $sums

        # The archive's name holds the version, so it comes from checksums.txt.
        $suffix = "_windows_$arch.zip"
        $lines = @(Get-Content $sums | ForEach-Object { , ($_ -split '\s+') } |
            Where-Object { $_.Count -eq 2 -and $_[1].StartsWith('ghostbranch_') -and $_[1].EndsWith($suffix) })
        if ($lines.Count -ne 1) { throw "ghostbranch install: the release lists $($lines.Count) archives for windows_$arch, not one" }
        $want, $archive = $lines[0][0], $lines[0][1]
        $version = $archive.Substring('ghostbranch_'.Length, $archive.Length - 'ghostbranch_'.Length - $suffix.Length)
        Assert-Version $version 'the version in checksums.txt'

        $zip = Join-Path $tmp $archive
        Get-File "$base/download/v$version/$archive" $zip
        $got = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash.ToLowerInvariant()
        if ($got -ne $want.ToLowerInvariant()) {
            Remove-Item -Force $zip
            throw "ghostbranch install: checksum mismatch for $archive (expected $want, got $got); nothing was installed"
        }
        Write-Host "Verified $archive against checksums.txt."

        $x = Join-Path $tmp 'x'
        Expand-Archive -Path $zip -DestinationPath $x
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $exe = Join-Path $dir 'ghostbranch.exe'
        $new = "$exe.new"
        $old = "$exe.old"
        Remove-Item -Recurse -Force $new -ErrorAction SilentlyContinue # a stale staging file
        Copy-Item (Join-Path $x 'ghostbranch.exe') $new
        # A running ghostbranch.exe can't be overwritten but can be renamed.
        $moved = $false
        if (Test-Path $exe) {
            Remove-Item -Recurse -Force $old -ErrorAction SilentlyContinue
            Move-Item -Force $exe $old
            $moved = $true
        }
        try {
            Move-Item -Force $new $exe
        } catch {
            if ($moved) { Move-Item -Force $old $exe -ErrorAction SilentlyContinue }
            throw
        }
        Remove-Item -Recurse -Force $old -ErrorAction SilentlyContinue

        $prev = $env:GHOSTBRANCH_NO_UPDATE_CHECK
        $env:GHOSTBRANCH_NO_UPDATE_CHECK = '1'
        $ok = $true
        try {
            $ErrorActionPreference = 'Continue'
            $text = @(& $exe version 2>&1 | ForEach-Object { "$_" })
            $installed = if ($text.Count) { $text[0].Trim() } else { "" }
            if ($LASTEXITCODE -ne 0) { $ok = $false }
        } catch {
            $ok = $false
            $installed = $_.Exception.Message
        } finally {
            $ErrorActionPreference = 'Stop'
            $env:GHOSTBRANCH_NO_UPDATE_CHECK = $prev
        }
        if ($ok) {
            Write-Host "Installed $installed"
        } else {
            Write-Host "Warning: $exe was installed but doesn't run on this machine ($installed)."
        }
        Write-Host "  at $exe"

        $norm = { param($p) $p.TrimEnd('\').ToLowerInvariant() }
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $entries = @(if ($userPath) { $userPath -split ';' | Where-Object { $_ } })
        if (-not ($entries | Where-Object { (& $norm $_) -eq (& $norm $dir) })) {
            if ($env:GHOSTBRANCH_TEST_SKIP_PATH -eq '1') {
                Write-Host "Would add $dir to the user PATH."
            } else {
                [Environment]::SetEnvironmentVariable('Path', (@($entries) + $dir) -join ';', 'User')
                $env:Path = "$env:Path;$dir"
                Write-Host "Added $dir to your user PATH. Open a new terminal to use ghostbranch."
            }
        }
        Write-Host ''
        Write-Host 'Next: ghostbranch login, then ghostbranch init in your repo.'
    } finally {
        if ($new) { Remove-Item -Recurse -Force $new -ErrorAction SilentlyContinue }
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
}

Install-Ghostbranch
