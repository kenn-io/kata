# kata installer for Windows
# Usage: powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"

$ErrorActionPreference = 'Stop'

$repo = 'kenn-io/kata'
$binaryName = 'kata.exe'

function Write-Info($msg) { Write-Host $msg -ForegroundColor Green }
function Write-Warn($msg) { Write-Host $msg -ForegroundColor Yellow }
function Write-Err($msg) { Write-Host $msg -ForegroundColor Red }

function Test-EnvBool($name) {
    $val = [Environment]::GetEnvironmentVariable($name)
    return ($val -match '^(1|true|yes)$')
}

function Get-Architecture {
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') {
        return 'arm64'
    }

    try {
        $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
        switch ($arch.ToString()) {
            'X64' { return 'amd64' }
            'X86' { return '386' }
            'Arm64' { return 'arm64' }
            default { return 'amd64' }
        }
    } catch {
        if ([System.Environment]::Is64BitOperatingSystem) {
            return 'amd64'
        } else {
            return '386'
        }
    }
}

function Invoke-WebRequestCompat {
    param([string]$Uri, [string]$OutFile)

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }

    $params = @{ Uri = $Uri }
    if ($OutFile) { $params.OutFile = $OutFile }

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    if ($OutFile) {
        Invoke-WebRequest @params
    } else {
        Invoke-RestMethod @params
    }
}

function Get-FinalUrl {
    param($Response)
    try {
        if ($Response.BaseResponse.ResponseUri) {
            return $Response.BaseResponse.ResponseUri.AbsoluteUri
        }
    } catch {}
    try {
        if ($Response.BaseResponse.RequestMessage.RequestUri) {
            return $Response.BaseResponse.RequestMessage.RequestUri.AbsoluteUri
        }
    } catch {}
    return $null
}

function Get-LatestVersion {
    $url = "https://github.com/$repo/releases/latest"

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }

    $params = @{
        Uri = $url
        Method = 'Head'
        ErrorAction = 'Stop'
    }
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    $finalUrl = $null
    try {
        $response = Invoke-WebRequest @params
        $finalUrl = Get-FinalUrl $response
    } catch {
        throw "Failed to fetch latest version: $_"
    }

    if (-not $finalUrl) {
        throw "Failed to fetch latest version: could not resolve release URL from $url"
    }
    if ($finalUrl -notmatch '/releases/tag/([^/]+)/?$') {
        throw "Failed to fetch latest version: unexpected release URL $finalUrl"
    }
    return $Matches[1]
}

function Test-ReleaseAsset {
    param([string]$Url)

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }

    $params = @{
        Uri = $Url
        Method = 'Head'
        ErrorAction = 'Stop'
    }
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    try {
        $response = Invoke-WebRequest @params
        return ($response.StatusCode -ge 200 -and $response.StatusCode -lt 300)
    } catch {
        return $false
    }
}

function Resolve-ReleaseArch {
    param([string]$DetectedArch, [string]$Version)

    $versionNum = $Version.TrimStart('v')
    $name = "kata_${versionNum}_windows_${DetectedArch}.zip"
    $url = "https://github.com/$repo/releases/download/$Version/$name"
    if (Test-ReleaseAsset $url) {
        return $DetectedArch
    }
    return $null
}

function Verify-Checksum {
    param([string]$ArchivePath, [string]$ChecksumFile, [string]$ArchiveName)

    $matchingLines = @()
    foreach ($line in Get-Content $ChecksumFile) {
        if ($line -match '^\s*$') { continue }
        $parts = $line -split '\s+', 2
        if ($parts.Count -lt 2) { continue }
        $hash = $parts[0]
        $filename = $parts[1]
        $filename = $filename -replace '^[\*]', ''
        $filename = $filename -replace '^\.\/', ''
        $filename = $filename -replace '^\.\\', ''
        if ($filename -eq $ArchiveName) {
            $matchingLines += $hash
        }
    }

    if ($matchingLines.Count -eq 0) {
        throw "Could not find checksum for $ArchiveName in SHA256SUMS"
    }

    if ($matchingLines.Count -gt 1) {
        throw "Multiple checksum entries found for $ArchiveName"
    }

    $expectedHash = $matchingLines[0]
    $actualHash = (Get-FileHash -Path $ArchivePath -Algorithm SHA256).Hash.ToLower()

    if ($actualHash -ne $expectedHash.ToLower()) {
        throw "Checksum verification failed! Expected: $expectedHash Got: $actualHash"
    }
}

function Get-InstallDir {
    if ($env:KATA_INSTALL_DIR) {
        return $env:KATA_INSTALL_DIR
    }
    return Join-Path $env:USERPROFILE '.kata\bin'
}

function Add-ToPath($dir) {
    $currentPath = [Environment]::GetEnvironmentVariable('Path', 'User')

    $normalizedDir = $dir.TrimEnd('\', '/')
    $alreadyInPath = $currentPath -split ';' | Where-Object {
        $_.TrimEnd('\', '/') -ieq $normalizedDir
    }
    if ($alreadyInPath) {
        Write-Info "Directory already in PATH"
        return $false
    }

    $newPath = "$currentPath;$dir"
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    $env:Path = "$env:Path;$dir"

    return $true
}

function Install-Kata {
    Write-Info "Installing kata..."
    Write-Host ""

    $arch = Get-Architecture
    Write-Info "Platform: windows/$arch"

    if ($arch -eq '386') {
        Write-Err "Error: 32-bit Windows is not supported."
        Write-Err "kata requires 64-bit Windows (amd64 or arm64)."
        exit 1
    }

    $version = Get-LatestVersion
    Write-Info "Latest version: $version"

    $resolvedArch = Resolve-ReleaseArch -DetectedArch $arch -Version $version
    if (-not $resolvedArch) {
        Write-Err "Error: No Windows release asset found for $version (detected windows/$arch)."
        Write-Err "See https://github.com/$repo for build-from-source instructions."
        exit 1
    }
    $arch = $resolvedArch

    $versionNum = $version.TrimStart('v')
    $archiveName = "kata_${versionNum}_windows_${arch}.zip"
    $downloadUrl = "https://github.com/$repo/releases/download/$version/$archiveName"

    $installDir = Get-InstallDir
    Write-Info "Install directory: $installDir"
    Write-Host ""

    if (-not (Test-Path $installDir)) {
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    }

    $tmpDir = Join-Path $env:TEMP "kata-install-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

    try {
        $archivePath = Join-Path $tmpDir $archiveName

        Write-Info "Downloading $archiveName..."
        Invoke-WebRequestCompat -Uri $downloadUrl -OutFile $archivePath

        $checksumUrl = "https://github.com/$repo/releases/download/$version/SHA256SUMS"
        $checksumFile = Join-Path $tmpDir "SHA256SUMS"

        Write-Info "Verifying checksum..."
        try {
            Invoke-WebRequestCompat -Uri $checksumUrl -OutFile $checksumFile
        } catch {
            Write-Err "Error: Could not download checksums file: $_"
            exit 1
        }

        try {
            Verify-Checksum -ArchivePath $archivePath -ChecksumFile $checksumFile -ArchiveName $archiveName
        } catch {
            Write-Err "Error: $_"
            exit 1
        }
        Write-Info "Checksum verified."

        Write-Info "Extracting..."
        if ($PSVersionTable.PSVersion.Major -lt 5) {
            Write-Err "Error: PowerShell 5.0 or later is required for Expand-Archive."
            Write-Err "Please upgrade PowerShell or download the release manually from GitHub."
            exit 1
        }
        try {
            Expand-Archive -Path $archivePath -DestinationPath $tmpDir -Force
        } catch {
            Write-Err "Error: Failed to extract archive: $_"
            exit 1
        }

        $binaryFile = Get-ChildItem -Path $tmpDir -Recurse -Filter $binaryName | Select-Object -First 1
        if (-not $binaryFile) {
            Write-Err "Error: Could not find $binaryName in extracted archive"
            exit 1
        }

        $destPath = Join-Path $installDir $binaryName

        if (Test-Path $destPath) {
            Remove-Item $destPath -Force
        }

        Move-Item $binaryFile.FullName $destPath -Force

        Write-Host ""
        Write-Info "Installation complete!"
        Write-Host ""

        if (-not (Test-EnvBool 'KATA_NO_MODIFY_PATH')) {
            $pathUpdated = Add-ToPath $installDir
            if ($pathUpdated) {
                Write-Info "Added $installDir to PATH"
                Write-Warn "Restart your terminal for PATH changes to take effect."
                Write-Host ""
            }
        }

        Write-Host "Check the install:"
        Write-Host "  kata version"
        Write-Host "  kata update --check"
        Write-Host ""
        Write-Host "Get started:"
        Write-Host "  cd your-repo"
        Write-Host "  kata init"
        Write-Host "  kata tui"

    } finally {
        if (Test-Path $tmpDir) {
            Remove-Item $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

if ($MyInvocation.InvocationName -ne '.') {
    Install-Kata
}
