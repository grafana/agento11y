#Requires -Version 5.1
$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot 'install.ps1'
$root = Join-Path ([IO.Path]::GetTempPath()) ('agento11y-installer-tests-' + [guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($root)
$originalPath = $env:PATH
$originalToken = $env:GITHUB_TOKEN
$env:GITHUB_TOKEN = 'installer-test-token'
$script:passed = 0
$script:failed = 0
Add-Type -AssemblyName System.IO.Compression.FileSystem

function Assert-Equal {
    param($Actual, $Expected)
    if ($Actual -cne $Expected) { throw "Expected '$Expected', got '$Actual'." }
}

function Assert-Throws {
    param([scriptblock]$Action, [string]$Pattern, [string]$Absent)
    try { & $Action } catch {
        if ($Absent -and $_.Exception.Message.Contains($Absent)) { throw 'The error exposed forbidden text.' }
        if ($_.Exception.Message -notlike $Pattern) { throw "Unexpected error: $($_.Exception.Message)" }
        return
    }
    throw "Expected an error matching '$Pattern'."
}

function Test-Case {
    param([string]$Name, [scriptblock]$Action)
    $tokenBefore = $env:GITHUB_TOKEN
    try {
        & {
            . $installer
            function Invoke-RestMethod { throw 'Unexpected network request.' }
            function Invoke-WebRequest { throw 'Unexpected network request.' }
            function Open-Agento11yUserEnvironment { throw 'Unexpected user registry access.' }
            function Send-Agento11yEnvironmentChanged { throw 'Unexpected environment broadcast.' }
            & $Action
        }
        $script:passed++
        Write-Host "PASS $Name"
    } catch {
        $script:failed++
        Write-Host "FAIL $Name : $_"
        Write-Host $_.ScriptStackTrace
    } finally {
        $env:PATH = $originalPath
        $env:GITHUB_TOKEN = $tokenBefore
    }
}

function New-TestArchive {
    param([string]$Path, [string[]]$Entries = @('agento11y.exe'))
    $zip = [IO.Compression.ZipFile]::Open($Path, [IO.Compression.ZipArchiveMode]::Create)
    try {
        foreach ($name in $Entries) {
            $entry = $zip.CreateEntry($name)
            $writer = New-Object IO.StreamWriter($entry.Open())
            try { $writer.Write('new-binary') } finally { $writer.Dispose() }
        }
    } finally { $zip.Dispose() }
}

try {
    Test-Case 'release selection excludes SDKs, drafts and prereleases' {
        function Invoke-RestMethod {
            param($Uri, $Headers, $TimeoutSec, $ErrorAction)
            Assert-Equal $Headers.Authorization 'Bearer installer-test-token'
            Assert-Equal $TimeoutSec 60
            return ,@(
                @{ tag_name = 'sdk-python/v9.0.0'; draft = $false; prerelease = $false },
                @{ tag_name = 'plugins/agento11y/v3.0.0'; draft = $true; prerelease = $false },
                @{ tag_name = 'plugins/agento11y/v2.0.0'; draft = $false; prerelease = $true },
                @{ tag_name = 'plugins/agento11y/v1.0.0-rc1'; draft = $false; prerelease = $false },
                @{ tag_name = 'plugins/agento11y/v0.51.0'; draft = $false; prerelease = $false }
            )
        }
        Assert-Equal (Get-Agento11yReleaseVersion) '0.51.0'
    }

    Test-Case 'release selection paginates beyond SDK releases' {
        function Invoke-RestMethod {
            param($Uri, $Headers, $TimeoutSec, $ErrorAction)
            if ($Uri -like '*page=1') { return ,@(@{ tag_name = 'sdk-js/v9.0.0' }) }
            Assert-Equal $Uri 'https://api.github.com/repos/grafana/agento11y/releases?per_page=100&page=2'
            return ,@(@{ tag_name = 'plugins/agento11y/v0.51.0' })
        }
        Assert-Equal (Get-Agento11yReleaseVersion) '0.51.0'
    }

    Test-Case 'empty release page stops the search' {
        function Invoke-RestMethod { return ,@() }
        Assert-Throws { Get-Agento11yReleaseVersion } '*No stable*'
    }

    Test-Case 'release pagination stops after ten pages' {
        $script:pages = 0
        function Invoke-RestMethod { $script:pages++; return ,@(@{ tag_name = 'sdk-js/v1.0.0' }) }
        Assert-Throws { Get-Agento11yReleaseVersion } '*No stable*'
        Assert-Equal $script:pages 10
    }

    Test-Case 'API failures give recovery without leaking the token' {
        function Invoke-RestMethod { throw 'installer-test-token' }
        Assert-Throws { Get-Agento11yReleaseVersion } '*set GITHUB_TOKEN*' -Absent 'installer-test-token'
    }

    Test-Case 'anonymous API requests omit authorization' {
        $env:GITHUB_TOKEN = ''
        function Invoke-RestMethod {
            param($Uri, $Headers, $TimeoutSec, $ErrorAction)
            Assert-Equal $Headers.ContainsKey('Authorization') $false
            return ,@(@{ tag_name = 'plugins/agento11y/v0.51.0' })
        }
        Assert-Equal (Get-Agento11yReleaseVersion) '0.51.0'
    }

    Test-Case 'downloads do not receive the API authorization header' {
        function Invoke-WebRequest {
            param([switch]$UseBasicParsing, $Uri, $OutFile, $TimeoutSec, $ErrorAction)
            Assert-Equal $Uri 'https://github.com/fixture.zip'
            Assert-Equal $OutFile 'fixture.zip'
            Assert-Equal $TimeoutSec 120
            Assert-Equal $UseBasicParsing.IsPresent $true
            Assert-Equal $args.Count 0
        }
        Save-Agento11yDownload -Uri 'https://github.com/fixture.zip' -Destination 'fixture.zip'
    }

    $script:fixtureArchive = Join-Path $root 'agento11y_0.51.0_windows_arm64.zip'
    New-TestArchive $script:fixtureArchive
    $script:fixtureHash = (Get-FileHash -LiteralPath $script:fixtureArchive -Algorithm SHA256).Hash
    $script:fixtureName = [IO.Path]::GetFileName($script:fixtureArchive)
    $checksumCases = @(
        @{ Name = 'LF checksum'; Text = "$script:fixtureHash  $script:fixtureName`n"; Error = '' },
        @{ Name = 'CRLF checksum'; Text = "$script:fixtureHash  $script:fixtureName`r`n"; Error = '' },
        @{ Name = 'binary checksum marker'; Text = "$($script:fixtureHash.ToLower()) *$script:fixtureName`n"; Error = '' },
        @{ Name = 'missing checksum'; Text = "$script:fixtureHash  another.zip`n"; Error = '*exactly one*' },
        @{ Name = 'filename suffix'; Text = "$script:fixtureHash  $script:fixtureName.extra`n"; Error = '*exactly one*' },
        @{ Name = 'duplicate checksum'; Text = "$script:fixtureHash  $script:fixtureName`n$script:fixtureHash  $script:fixtureName`n"; Error = '*exactly one*' },
        @{ Name = 'malformed checksum'; Text = "123  $script:fixtureName`n"; Error = '*exactly one*' },
        @{ Name = 'wrong checksum'; Text = "$('0' * 64)  $script:fixtureName`n"; Error = '*mismatch*' }
    )
    foreach ($case in $checksumCases) {
        Test-Case $case.Name {
            $checksums = Join-Path $root 'checksum-test.txt'
            [IO.File]::WriteAllText($checksums, $case.Text)
            if ($case.Error) {
                Assert-Throws { Test-Agento11yChecksum -Archive $script:fixtureArchive -Checksums $checksums } $case.Error
            } else {
                Test-Agento11yChecksum -Archive $script:fixtureArchive -Checksums $checksums
            }
        }
    }

    foreach ($entries in @(@('agento11y.exe', '../outside.exe', 'LICENSE'), @('nested/agento11y.exe'), @('agento11y.exe', 'agento11y.exe'))) {
        Test-Case "extract only the unique root executable: $entries" {
            $zipPath = Join-Path $root ([guid]::NewGuid().ToString() + '.zip')
            $output = Join-Path $root ([guid]::NewGuid().ToString() + '.exe')
            New-TestArchive -Path $zipPath -Entries $entries
            if ($entries.Count -eq 3) {
                Expand-Agento11yExecutable -Archive $zipPath -Destination $output
                Assert-Equal ([IO.File]::ReadAllText($output)) 'new-binary'
                Assert-Equal (Test-Path -LiteralPath (Join-Path $root 'outside.exe')) $false
            } else {
                Assert-Throws { Expand-Agento11yExecutable -Archive $zipPath -Destination $output } '*one nonempty*'
                Assert-Equal (Test-Path -LiteralPath $output) $false
            }
        }
    }

    $pathCases = @(
        @{ Value = ''; Directory = 'C:\New Bin'; Want = 'C:\New Bin' },
        @{ Value = 'C:\Windows'; Directory = 'C:\New Bin'; Want = 'C:\Windows;C:\New Bin' },
        @{ Value = 'C:\Windows;'; Directory = 'C:\New Bin'; Want = 'C:\Windows;C:\New Bin' },
        @{ Value = 'C:\NEW BIN\;C:\Windows'; Directory = 'c:\new bin'; Want = 'C:\NEW BIN\;C:\Windows' },
        @{ Value = 'C:/New Bin/;C:\Windows'; Directory = 'C:\New Bin'; Want = 'C:/New Bin/;C:\Windows' },
        @{ Value = '"C:\New Bin";C:\Windows'; Directory = 'C:\New Bin'; Want = '"C:\New Bin";C:\Windows' },
        @{ Value = 'C:\New Binary'; Directory = 'C:\New Bin'; Want = 'C:\New Binary;C:\New Bin' }
    )
    foreach ($case in $pathCases) {
        Test-Case "PATH preserves entries: $($case.Value)" {
            Assert-Equal (Add-Agento11yPathDirectory $case.Value $case.Directory) $case.Want
        }
    }

    if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        foreach ($case in @(@{ Architecture = 9; Want = 'amd64' }, @{ Architecture = 12; Want = 'arm64' }, @{ Architecture = 0; Want = '' })) {
            Test-Case "native processor architecture $($case.Architecture)" {
                function Get-CimInstance { return [pscustomobject]@{ Architecture = $case.Architecture } }
                if ($case.Want) { Assert-Equal (Get-Agento11yArchitecture) $case.Want } else {
                    Assert-Throws { Get-Agento11yArchitecture } '*Unsupported*'
                }
            }
        }
        $registryCases = @(
            @{ Name = 'literal'; Kind = 'String'; Before = '%USERPROFILE%\custom;C:\keep'; Directory = 'C:\New Bin'; After = '%USERPROFILE%\custom;C:\keep;C:\New Bin'; NotifyFails = $false },
            @{ Name = 'expandable'; Kind = 'ExpandString'; Before = '%USERPROFILE%\custom;C:\keep'; Directory = 'C:\New Bin'; After = '%USERPROFILE%\custom;C:\keep;C:\New Bin'; NotifyFails = $false },
            @{ Name = 'absent'; Kind = 'ExpandString'; Before = $null; Directory = 'C:\New Bin'; After = 'C:\New Bin'; NotifyFails = $false },
            @{ Name = 'existing reference'; Kind = 'ExpandString'; Before = '%USERPROFILE%\custom'; Directory = "$env:USERPROFILE\custom"; After = '%USERPROFILE%\custom'; NotifyFails = $false },
            @{ Name = 'notification failure'; Kind = 'ExpandString'; Before = ''; Directory = 'C:\New Bin'; After = 'C:\New Bin'; NotifyFails = $true }
        )
        foreach ($registryCase in $registryCases) {
            Test-Case "user PATH: $($registryCase.Name)" {
                $keyName = 'Software\Agento11yInstallerTest-' + [guid]::NewGuid().ToString('N')
                function Open-Agento11yUserEnvironment { return [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($keyName) }
                function Send-Agento11yEnvironmentChanged { if ($registryCase.NotifyFails) { throw 'fixture notification failure' } }
                $key = Open-Agento11yUserEnvironment
                try {
                    if ($null -ne $registryCase.Before) { $key.SetValue('Path', $registryCase.Before, [Microsoft.Win32.RegistryValueKind]$registryCase.Kind) }
                } finally { $key.Dispose() }
                try {
                    $env:PATH = 'C:\session-only'
                    $WarningPreference = 'Continue'
                    $warnings = @(Update-Agento11yPath -Directory $registryCase.Directory 3>&1)
                    Update-Agento11yPath -Directory $registryCase.Directory 3>$null
                    if ($registryCase.NotifyFails) { Assert-Equal $warnings.Count 1 } else { Assert-Equal $warnings.Count 0 }
                    Assert-Equal $env:PATH "C:\session-only;$($registryCase.Directory)"
                    $key = Open-Agento11yUserEnvironment
                    try {
                        Assert-Equal ($key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)) $registryCase.After
                        Assert-Equal ($key.GetValueKind('Path')) ([Microsoft.Win32.RegistryValueKind]$registryCase.Kind)
                    } finally { $key.Dispose() }
                } finally { [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree($keyName) }
            }
        }
        Test-Case 'PATH recognizes an expanded directory without rewriting it' {
            Assert-Equal (Add-Agento11yPathDirectory '%USERPROFILE%\custom' "$env:USERPROFILE\custom" -ExpandReferences) '%USERPROFILE%\custom'
            Assert-Equal (Add-Agento11yPathDirectory '%USERPROFILE%\custom' "$env:USERPROFILE\custom") "%USERPROFILE%\custom;$env:USERPROFILE\custom"
        }
    } else {
        Test-Case 'unsupported operating system fails before networking' {
            Assert-Throws { Get-Agento11yArchitecture } '*Windows only*'
        }
        Write-Host 'SKIP Windows architecture, registry and file-lock checks on this OS.'
    }

    foreach ($scenario in @('install', 'upgrade', 'latest release', 'shadowed command', 'checksum failure', 'download failure', 'empty archive', 'corrupt archive', 'invalid executable', 'PATH failure', 'locked executable')) {
        if ($scenario -eq 'locked executable' -and [Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { continue }
        Test-Case "installer: $scenario" {
            $directory = Join-Path $root ('install space ' + [guid]::NewGuid().ToString('N'))
            [void][IO.Directory]::CreateDirectory($directory)
            $destination = Join-Path $directory 'agento11y.exe'
            if ($scenario -ne 'install') { [IO.File]::WriteAllText($destination, 'old-binary') }
            $script:downloadCount = 0
            $script:downloadHash = $script:fixtureHash
            $script:pathUpdated = $false
            function Get-Agento11yArchitecture { return 'arm64' }
            function Get-Agento11yReleaseVersion {
                if ($scenario -eq 'latest release') { return '0.51.0' }
                throw 'Pinned version must not use the API.'
            }
            function Save-Agento11yDownload {
                param($Uri, $Destination)
                $script:downloadCount++
                $script:lastDownloadDirectory = [IO.Path]::GetDirectoryName($Destination)
                if ($scenario -eq 'download failure') { throw 'fixture download failure' }
                if ($script:downloadCount -eq 1) {
                    Assert-Equal $Uri 'https://github.com/grafana/agento11y/releases/download/plugins/agento11y/v0.51.0/agento11y_0.51.0_windows_arm64.zip'
                    switch ($scenario) {
                        'empty archive' { New-TestArchive -Path $Destination -Entries @() }
                        'corrupt archive' { [IO.File]::WriteAllText($Destination, 'not a zip') }
                        default { [IO.File]::Copy($script:fixtureArchive, $Destination) }
                    }
                    $script:downloadHash = (Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash
                } else {
                    Assert-Equal $Uri 'https://github.com/grafana/agento11y/releases/download/plugins/agento11y/v0.51.0/agento11y_0.51.0_checksums.txt'
                    $expected = $script:downloadHash
                    if ($scenario -eq 'checksum failure') { $expected = '0' * 64 }
                    [IO.File]::WriteAllText($Destination, "$expected  $script:fixtureName`n")
                }
            }
            function Test-Agento11yExecutable {
                param($Path, $Version)
                Assert-Equal ([IO.File]::ReadAllText($Path)) 'new-binary'
                Assert-Equal $Version '0.51.0'
                if ($scenario -eq 'invalid executable') { throw 'fixture invalid executable' }
            }
            function Update-Agento11yPath {
                param($Directory)
                if ($scenario -eq 'PATH failure') { throw 'fixture registry failure' }
                $script:pathUpdated = $true
            }
            function Get-Command {
                if ($scenario -eq 'shadowed command') {
                    return [pscustomobject]@{ CommandType = 'Application'; Path = 'C:\older\agento11y.exe'; Definition = 'C:\older\agento11y.exe' }
                }
                return $null
            }
            $lock = $null
            $tls = [Net.ServicePointManager]::SecurityProtocol
            try {
                if ($scenario -eq 'locked executable') {
                    $lock = [IO.File]::Open($destination, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
                }
                switch ($scenario) {
                    'latest release' { Install-Agento11y -InstallDir $directory }
                    'shadowed command' {
                        Install-Agento11y -Version v0.51.0 -InstallDir $directory -WarningVariable warnings -WarningAction SilentlyContinue
                        Assert-Equal ($warnings -like '*takes precedence*').Count 1
                    }
                    'checksum failure' { Assert-Throws { Install-Agento11y -Version v0.51.0 -InstallDir $directory } '*checksum mismatch*' }
                    'download failure' { Assert-Throws { Install-Agento11y -Version v0.51.0 -InstallDir $directory } '*download failure*' }
                    'empty archive' { Assert-Throws { Install-Agento11y -Version v0.51.0 -InstallDir $directory } '*one nonempty*' }
                    'corrupt archive' { Assert-Throws { Install-Agento11y -Version v0.51.0 -InstallDir $directory } '*OpenRead*' }
                    'invalid executable' { Assert-Throws { Install-Agento11y -Version v0.51.0 -InstallDir $directory } '*invalid executable*' }
                    'PATH failure' { Assert-Throws { Install-Agento11y -Version v0.51.0 -InstallDir $directory } '*Installed*but could not update PATH*' }
                    'locked executable' { Assert-Throws { Install-Agento11y -Version v0.51.0 -InstallDir $directory } '*Could not replace*' }
                    default { Install-Agento11y -Version v0.51.0 -InstallDir $directory }
                }
            } finally { if ($lock) { $lock.Dispose() } }
            $installed = $scenario -in @('install', 'upgrade', 'latest release', 'shadowed command', 'PATH failure')
            if ($installed) { Assert-Equal ([IO.File]::ReadAllText($destination)) 'new-binary' } else {
                Assert-Equal ([IO.File]::ReadAllText($destination)) 'old-binary'
            }
            Assert-Equal $script:pathUpdated ($scenario -in @('install', 'upgrade', 'latest release', 'shadowed command'))
            Assert-Equal @(Get-ChildItem -LiteralPath $directory -Force).Count 1
            Assert-Equal (Test-Path -LiteralPath $script:lastDownloadDirectory) $false
            Assert-Equal ([Net.ServicePointManager]::SecurityProtocol) $tls
        }
    }

    foreach ($state in @('before move', 'after move', 'destination reappeared', 'restore blocked')) {
        Test-Case "replacement failure: $state" {
            $directory = Join-Path $root ([guid]::NewGuid().ToString('N'))
            [void][IO.Directory]::CreateDirectory($directory)
            $staged = Join-Path $directory 'new.exe'
            $destination = Join-Path $directory 'agento11y.exe'
            [IO.File]::WriteAllText($staged, 'new-binary')
            [IO.File]::WriteAllText($destination, 'old-binary')
            $replace = {
                param($Source, $Target, $Backup)
                if ($state -ne 'before move') { [IO.File]::Move($Target, $Backup) }
                if ($state -eq 'destination reappeared') { [IO.File]::WriteAllText($Target, 'another-writer') }
                if ($state -eq 'restore blocked') { [void][IO.Directory]::CreateDirectory($Target) }
                throw 'fixture replacement failure'
            }
            $WarningPreference = 'SilentlyContinue'
            Assert-Throws { Set-Agento11yExecutable -Staged $staged -Destination $destination -ReplaceFile $replace } '*Could not replace*'
            if ($state -in @('before move', 'after move')) {
                Assert-Equal ([IO.File]::ReadAllText($destination)) 'old-binary'
                Assert-Equal (Test-Path -LiteralPath "$staged.backup") $false
            } else {
                Assert-Equal ([IO.File]::ReadAllText("$staged.backup")) 'old-binary'
                if ($state -eq 'destination reappeared') { Assert-Equal ([IO.File]::ReadAllText($destination)) 'another-writer' }
            }
        }
    }
    Test-Case 'identical executable does not need replacement' {
        $staged = Join-Path $root 'identical-new.exe'
        $destination = Join-Path $root 'identical-existing.exe'
        [IO.File]::WriteAllText($staged, 'same-binary')
        [IO.File]::WriteAllText($destination, 'same-binary')
        Set-Agento11yExecutable -Staged $staged -Destination $destination -ReplaceFile { throw 'Unexpected replacement.' }
        Assert-Equal ([IO.File]::ReadAllText($destination)) 'same-binary'
        Assert-Equal (Test-Path -LiteralPath $staged) $true
    }
    Test-Case 'destination directory is not replaced' {
        $directory = Join-Path $root 'destination-directory'
        [void][IO.Directory]::CreateDirectory($directory)
        $staged = Join-Path $root 'directory-test.exe'
        [IO.File]::WriteAllText($staged, 'new-binary')
        Assert-Throws { Set-Agento11yExecutable -Staged $staged -Destination $directory } '*Could not replace*'
        Assert-Equal ([IO.Directory]::Exists($directory)) $true
    }

    Test-Case 'script entry supports Invoke-Expression, files and dot-sourcing' {
        $source = Get-Content -LiteralPath $installer -Raw
        $ast = [Management.Automation.Language.Parser]::ParseInput($source, [ref]$null, [ref]$null)
        $main = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Install-Agento11y' }, $true)
        $entry = $source.Replace($main.Extent.Text, 'function Install-Agento11y { param($Version, $InstallDir) [pscustomobject]@{ Version = $Version; InstallDir = $InstallDir } }')
        $entryPath = Join-Path $root 'entry-test.ps1'
        [IO.File]::WriteAllText($entryPath, $entry)
        $oldVersion = $env:VERSION
        $oldInstallDir = $env:INSTALL_DIR
        try {
            $env:VERSION = '0.51.0'
            $env:INSTALL_DIR = 'C:\from environment'
            $result = @(Invoke-Expression $entry)
            Assert-Equal $result.Count 1
            Assert-Equal $result[0].Version '0.51.0'
            Assert-Equal $result[0].InstallDir 'C:\from environment'
            $result = @(& $entryPath -Version v0.50.0 -InstallDir 'C:\from parameters')
            Assert-Equal $result.Count 1
            Assert-Equal $result[0].Version 'v0.50.0'
            Assert-Equal $result[0].InstallDir 'C:\from parameters'
            $result = @(. $entryPath)
            Assert-Equal $result.Count 0
        } finally {
            $env:VERSION = $oldVersion
            $env:INSTALL_DIR = $oldInstallDir
        }
    }

    foreach ($testVersion in @('../0.51.0', 'latest', '0.51.0-rc1', '0.51.0?query')) {
        Test-Case "reject invalid version $testVersion" {
            function Get-Agento11yArchitecture { return 'amd64' }
            Assert-Throws { Install-Agento11y -Version $testVersion -InstallDir $root } '*Version must*'
        }
    }
    foreach ($case in @(@{ Code = 0; Output = 'v0.51.0'; Fails = $false }, @{ Code = 1; Output = 'v0.51.0'; Fails = $true }, @{ Code = 0; Output = 'v0.50.0'; Fails = $true })) {
        Test-Case "executable version check: $($case.Code), $($case.Output)" {
            function Invoke-TestBinary { $global:LASTEXITCODE = $case.Code; return $case.Output }
            if ($case.Fails) {
                Assert-Throws { Test-Agento11yExecutable -Path Invoke-TestBinary -Version 0.51.0 } '*expected version*'
            } else {
                Test-Agento11yExecutable -Path Invoke-TestBinary -Version 0.51.0
            }
        }
    }
    foreach ($suffix in @('semi;colon', 'percent%path', "line`nbreak")) {
        Test-Case "reject PATH-incompatible installation directory $suffix" {
            function Get-Agento11yArchitecture { return 'amd64' }
            Assert-Throws { Install-Agento11y -Version 0.51.0 -InstallDir (Join-Path $root $suffix) } '*directory cannot contain*'
        }
    }
} finally {
    $env:PATH = $originalPath
    $env:GITHUB_TOKEN = $originalToken
    Remove-Item -LiteralPath $root -Recurse -Force
}
Write-Host "$script:passed passed; $script:failed failed."
if ($script:failed -ne 0) { throw 'PowerShell installer tests failed.' }
