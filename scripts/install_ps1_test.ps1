$ErrorActionPreference = 'Stop'

$script:InstallerPath = Join-Path $PSScriptRoot 'install.ps1'

function Fail($Message) {
    Write-Error "FAIL: $Message"
    exit 1
}

function Assert-Equal($Expected, $Actual, $Context) {
    if ($Expected -ne $Actual) {
        Fail "$Context expected [$Expected], got [$Actual]"
    }
}

function Assert-True($Condition, $Context) {
    if (-not $Condition) {
        Fail $Context
    }
}

function Assert-Throws($ExpectedMessage, [scriptblock]$Block, $Context) {
    try {
        & $Block
    } catch {
        if ($_.Exception.Message -notlike "*$ExpectedMessage*") {
            Fail "$Context expected error containing [$ExpectedMessage], got [$($_.Exception.Message)]"
        }
        return
    }
    Fail "$Context expected an error"
}

function Invoke-WebRequest {
    throw "installer unexpectedly ran while dot-sourced"
}

. $script:InstallerPath
$script:OriginalTestReleaseAsset = (Get-Command Test-ReleaseAsset).ScriptBlock

function Set-ReleaseAssetMock([scriptblock]$Mock) {
    Set-Item -Path function:script:Test-ReleaseAsset -Value $Mock
}

function Restore-ReleaseAssetMock {
    Set-Item -Path function:script:Test-ReleaseAsset -Value $script:OriginalTestReleaseAsset
}

function Test-DotSourceLoadsHelpersWithoutInstalling {
    Assert-True (Get-Command Resolve-ReleaseArch -ErrorAction SilentlyContinue) "Resolve-ReleaseArch should be loaded"
    Assert-True (Get-Command Verify-Checksum -ErrorAction SilentlyContinue) "Verify-Checksum should be loaded"
}

function Test-ResolveReleaseArchUsesDetectedWindowsArm64Asset {
    $script:ReleaseAssetUrls = @()
    Set-ReleaseAssetMock {
        param([string]$Url)
        $script:ReleaseAssetUrls += $Url
        return $true
    }

    try {
        $arch = Resolve-ReleaseArch -DetectedArch 'arm64' -Version 'v0.5.0'

        Assert-Equal 'arm64' $arch "arm64 asset selection"
        Assert-Equal 1 $script:ReleaseAssetUrls.Count "arm64 asset selection URL count"
        Assert-True ($script:ReleaseAssetUrls[0].EndsWith('/kata_0.5.0_windows_arm64.zip')) "arm64 asset selection URL"
    } finally {
        Restore-ReleaseAssetMock
    }
}

function Test-ResolveReleaseArchDoesNotFallbackFromArm64ToAmd64 {
    $script:ReleaseAssetUrls = @()
    Set-ReleaseAssetMock {
        param([string]$Url)
        $script:ReleaseAssetUrls += $Url
        return $false
    }

    try {
        $arch = Resolve-ReleaseArch -DetectedArch 'arm64' -Version 'v0.5.0'

        Assert-Equal $null $arch "missing arm64 asset selection"
        Assert-Equal 1 $script:ReleaseAssetUrls.Count "missing arm64 asset URL count"
        Assert-True ($script:ReleaseAssetUrls[0].EndsWith('/kata_0.5.0_windows_arm64.zip')) "missing arm64 asset URL"
    } finally {
        Restore-ReleaseAssetMock
    }
}

function Test-VerifyChecksumAcceptsMatchingHash {
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) "kata-install-ps1-test-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmp -Force | Out-Null
    try {
        $archiveName = 'kata_0.5.0_windows_amd64.zip'
        $archivePath = Join-Path $tmp $archiveName
        $checksumFile = Join-Path $tmp 'SHA256SUMS'
        Set-Content -LiteralPath $archivePath -Value 'archive payload' -NoNewline
        $hash = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()
        Set-Content -LiteralPath $checksumFile -Value "$hash  *$archiveName"

        Verify-Checksum -ArchivePath $archivePath -ChecksumFile $checksumFile -ArchiveName $archiveName
    } finally {
        Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Test-VerifyChecksumRejectsMismatch {
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) "kata-install-ps1-test-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmp -Force | Out-Null
    try {
        $archiveName = 'kata_0.5.0_windows_amd64.zip'
        $archivePath = Join-Path $tmp $archiveName
        $checksumFile = Join-Path $tmp 'SHA256SUMS'
        Set-Content -LiteralPath $archivePath -Value 'archive payload' -NoNewline
        Set-Content -LiteralPath $checksumFile -Value "0000000000000000000000000000000000000000000000000000000000000000  $archiveName"

        Assert-Throws 'Checksum verification failed' {
            Verify-Checksum -ArchivePath $archivePath -ChecksumFile $checksumFile -ArchiveName $archiveName
        } "checksum mismatch"
    } finally {
        Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Test-DotSourceLoadsHelpersWithoutInstalling
Test-ResolveReleaseArchUsesDetectedWindowsArm64Asset
Test-ResolveReleaseArchDoesNotFallbackFromArm64ToAmd64
Test-VerifyChecksumAcceptsMatchingHash
Test-VerifyChecksumRejectsMismatch

Write-Host "PowerShell installer tests passed"
