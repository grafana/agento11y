#Requires -Version 5.1
[CmdletBinding()]
param(
    [string]$Version = $env:VERSION,
    [string]$InstallDir = $env:INSTALL_DIR
)

function Get-Agento11yArchitecture {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'This installer supports Windows only. Use install.sh on Linux or macOS.'
    }
    if (-not [Environment]::Is64BitOperatingSystem) {
        throw 'This installer requires 64-bit Windows (x64 or ARM64).'
    }
    # The process architecture can be x86 or x64 on an ARM64 machine.
    $architecture = Get-CimInstance -ClassName Win32_Processor -ErrorAction Stop |
        Select-Object -First 1 -ExpandProperty Architecture
    switch ($architecture) {
        9 { return 'amd64' }
        12 { return 'arm64' }
        default { throw "Unsupported Windows processor architecture: $architecture. Expected x64 or ARM64." }
    }
}

function Get-Agento11yReleaseVersion {
    $headers = @{ Accept = 'application/vnd.github+json'; 'User-Agent' = 'agento11y-installer' }
    if ($env:GITHUB_TOKEN) {
        $headers.Authorization = "Bearer $env:GITHUB_TOKEN"
    }
    # SDK releases share this repository, so releases/latest can select another product.
    for ($page = 1; $page -le 10; $page++) {
        $uri = "https://api.github.com/repos/grafana/agento11y/releases?per_page=100&page=$page"
        try {
            $releases = Invoke-RestMethod -Uri $uri -Headers $headers -TimeoutSec 60 -ErrorAction Stop
        } catch {
            throw 'Could not fetch GitHub releases. Retry, set GITHUB_TOKEN for API rate limits, or specify -Version.'
        }
        foreach ($release in $releases) {
            if (-not $release.draft -and -not $release.prerelease -and
                $release.tag_name -cmatch '^plugins/agento11y/v([0-9]+\.[0-9]+\.[0-9]+)$') {
                return $Matches[1]
            }
        }
        if ($releases.Count -eq 0) { break }
    }
    throw 'No stable plugins/agento11y release found. Specify -Version to select a release.'
}

function Save-Agento11yDownload {
    param([string]$Uri, [string]$Destination)
    try {
        Invoke-WebRequest -UseBasicParsing -Uri $Uri -OutFile $Destination -TimeoutSec 120 -ErrorAction Stop
    } catch {
        throw "Could not download $Uri. Check the version and network access, then retry."
    }
}

function Test-Agento11yChecksum {
    param([string]$Archive, [string]$Checksums)
    $name = [IO.Path]::GetFileName($Archive)
    $pattern = '^([a-fA-F0-9]{64})\s+\*?' + [regex]::Escape($name) + '$'
    $hashes = @(foreach ($line in Get-Content -LiteralPath $Checksums) {
        if ($line -cmatch $pattern) { $Matches[1] }
    })
    if ($hashes.Count -ne 1) {
        throw "Expected exactly one SHA-256 checksum for $name. Nothing was installed."
    }
    $actual = (Get-FileHash -LiteralPath $Archive -Algorithm SHA256 -ErrorAction Stop).Hash
    if ($actual -ine $hashes[0]) {
        throw "SHA-256 checksum mismatch for $name. Nothing was installed."
    }
}

function Expand-Agento11yExecutable {
    param([string]$Archive, [string]$Destination)
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [IO.Compression.ZipFile]::OpenRead($Archive)
    try {
        $entries = @($zip.Entries | Where-Object { $_.FullName -ceq 'agento11y.exe' })
        if ($entries.Count -ne 1 -or $entries[0].Length -eq 0) {
            throw 'The archive must contain one nonempty agento11y.exe at its root.'
        }
        [IO.Compression.ZipFileExtensions]::ExtractToFile($entries[0], $Destination, $false)
    } finally {
        $zip.Dispose()
    }
}

function Test-Agento11yExecutable {
    param([string]$Path, [string]$Version)
    $output = & $Path --version
    if ($LASTEXITCODE -ne 0 -or ([string]$output).Trim() -ne "v$Version") {
        throw 'The downloaded executable did not report the expected version. Your existing installation is unchanged.'
    }
}

function Set-Agento11yExecutable {
    param(
        [string]$Staged,
        [string]$Destination,
        [scriptblock]$ReplaceFile = { param($Source, $Target, $Backup) [IO.File]::Replace($Source, $Target, $Backup) }
    )
    $backup = "$Staged.backup"
    try {
        $existing = Get-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue
        if ($existing -and ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint))) {
            throw 'The destination must be a regular file, not a directory or link.'
        }
        if ($existing) {
            if ((Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash -eq
                (Get-FileHash -LiteralPath $Staged -Algorithm SHA256).Hash) { return }
            # ReplaceFile can move the old file before failing. Keep a recovery copy.
            # https://learn.microsoft.com/windows/win32/api/winbase/nf-winbase-replacefilew
            & $ReplaceFile $Staged $Destination $backup
        } else {
            [IO.File]::Move($Staged, $Destination)
        }
    } catch {
        if ([IO.File]::Exists($backup)) {
            if (-not [IO.File]::Exists($Destination)) {
                try { [IO.File]::Move($backup, $Destination) } catch {
                    Write-Warning "Could not restore the previous executable. Its backup is at $backup."
                }
            } else {
                Write-Warning "The previous executable was kept at $backup."
            }
        }
        throw "Could not replace $Destination. Stop running agento11y processes, including the local daemon. Check directory permissions and whether the destination is a link. Then retry."
    }
    if ([IO.File]::Exists($backup)) {
        try { Remove-Item -LiteralPath $backup -Force -ErrorAction Stop } catch {
            Write-Warning "Installed the new executable, but could not remove the previous copy at $backup."
        }
    }
}

function Add-Agento11yPathDirectory {
    param([string]$PathValue, [string]$Directory, [switch]$ExpandReferences)
    foreach ($entry in ($PathValue -split ';')) {
        $candidate = $entry.Trim().Trim('"')
        if ($ExpandReferences) { $candidate = [Environment]::ExpandEnvironmentVariables($candidate) }
        if ($candidate.Replace('/', '\').TrimEnd('\') -ieq $Directory.Replace('/', '\').TrimEnd('\')) { return $PathValue }
    }
    if ([string]::IsNullOrEmpty($PathValue)) { return $Directory }
    if ($PathValue.EndsWith(';')) { return "$PathValue$Directory" }
    return "$PathValue;$Directory"
}

function Open-Agento11yUserEnvironment {
    return [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
}

function Send-Agento11yEnvironmentChanged {
    if (-not ('Agento11y.Installer.NativeMethods' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
namespace Agento11y.Installer {
    public static class NativeMethods {
        [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern IntPtr SendMessageTimeout(
            IntPtr window, uint message, UIntPtr wParam, string lParam,
            uint flags, uint timeout, out UIntPtr result);
    }
}
'@
    }
    $result = [UIntPtr]::Zero
    $sent = [Agento11y.Installer.NativeMethods]::SendMessageTimeout(
        [IntPtr]0xffff, 0x001a, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$result)
    if ($sent -eq [IntPtr]::Zero) {
        Write-Warning 'The user PATH was saved, but Windows could not notify every application. Sign out and back in if new terminals cannot find agento11y.'
    }
}

function Update-Agento11yPath {
    param([string]$Directory)
    $key = Open-Agento11yUserEnvironment
    try {
        # Environment.GetEnvironmentVariable expands registry references such as %USERPROFILE%.
        $path = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
        if ($key.GetValueNames() -contains 'Path') { $kind = $key.GetValueKind('Path') }
        $updated = Add-Agento11yPathDirectory -PathValue $path -Directory $Directory -ExpandReferences:($kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString)
        if ($updated -cne $path) { $key.SetValue('Path', $updated, $kind) }
    } finally {
        $key.Dispose()
    }
    $env:PATH = Add-Agento11yPathDirectory -PathValue $env:PATH -Directory $Directory
    try {
        Send-Agento11yEnvironmentChanged
    } catch {
        Write-Warning 'The user PATH was saved, but Windows could not be notified. Sign out and back in if new terminals cannot find agento11y.'
    }
}

function Install-Agento11y {
    [CmdletBinding()]
    param([string]$Version, [string]$InstallDir)
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'
    $architecture = Get-Agento11yArchitecture
    if ([string]::IsNullOrWhiteSpace($InstallDir)) {
        if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA is unavailable. Specify -InstallDir.' }
        $InstallDir = Join-Path $env:LOCALAPPDATA 'agento11y\bin'
    }
    $InstallDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($InstallDir)
    if ($InstallDir -match '[;"%\r\n]') {
        throw 'The installation directory cannot contain semicolons, quotes, percent signs or line breaks because it is added to PATH.'
    }
    if ($Version) {
        $Version = $Version -replace '^v', ''
        if ($Version -cnotmatch '^[0-9]+\.[0-9]+\.[0-9]+$') {
            throw 'Version must be a stable release number, for example 0.51.0 or v0.51.0.'
        }
    }

    $temporary = $null
    $staged = $null
    $tls = [Net.ServicePointManager]::SecurityProtocol
    try {
        # Windows PowerShell 5.1 can otherwise use TLS versions GitHub rejects.
        [Net.ServicePointManager]::SecurityProtocol = $tls -bor [Net.SecurityProtocolType]::Tls12
        if (-not $Version) { $Version = Get-Agento11yReleaseVersion }
        Write-Host "Installing agento11y v$Version (windows/$architecture)..."
        $temporary = Join-Path ([IO.Path]::GetTempPath()) ("agento11y-install-" + [guid]::NewGuid().ToString('N'))
        [void][IO.Directory]::CreateDirectory($temporary)
        $name = "agento11y_${Version}_windows_${architecture}.zip"
        $base = "https://github.com/grafana/agento11y/releases/download/plugins/agento11y/v$Version"
        $archive = Join-Path $temporary $name
        $checksums = Join-Path $temporary 'checksums.txt'
        Save-Agento11yDownload -Uri "$base/$name" -Destination $archive
        Save-Agento11yDownload -Uri "$base/agento11y_${Version}_checksums.txt" -Destination $checksums
        Test-Agento11yChecksum -Archive $archive -Checksums $checksums

        [void][IO.Directory]::CreateDirectory($InstallDir)
        $destination = Join-Path $InstallDir 'agento11y.exe'
        $staged = Join-Path $InstallDir ('.agento11y-' + [guid]::NewGuid().ToString('N') + '.exe')
        Expand-Agento11yExecutable -Archive $archive -Destination $staged
        Test-Agento11yExecutable -Path $staged -Version $Version
        Set-Agento11yExecutable -Staged $staged -Destination $destination
        try {
            Update-Agento11yPath -Directory $InstallDir
        } catch {
            throw "Installed $destination, but could not update PATH. Add $InstallDir to your user PATH, or fix permissions and rerun the installer."
        }
        Write-Host "Installed agento11y v$Version to $destination"
        $command = Get-Command agento11y -ErrorAction SilentlyContinue
        if ($command -and ($command.CommandType -ne 'Application' -or $command.Path -ine $destination)) {
            Write-Warning "Another agento11y command takes precedence: $($command.Definition). Remove that command or adjust PATH to use $destination."
        }
        Write-Host 'Run agento11y --version, then agento11y login.'
        Write-Host 'Restart other terminal applications to pick up the user PATH change.'
    } finally {
        [Net.ServicePointManager]::SecurityProtocol = $tls
        if ($staged -and (Test-Path -LiteralPath $staged)) { Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue }
        if ($temporary) { Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue }
    }
}

if ($MyInvocation.InvocationName -ne '.') {
    Install-Agento11y -Version $Version -InstallDir $InstallDir
}
