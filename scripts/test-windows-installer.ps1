param(
    [string]$Version = "installer-test"
)

$ErrorActionPreference = "Stop"

$RepositoryRoot = Split-Path -Parent $PSScriptRoot
$Suffix = [guid]::NewGuid().ToString("N")
$ReleaseDirectory = Join-Path $env:RUNNER_TEMP "hce-test-release-$Suffix"
$PackageDirectory = Join-Path $env:RUNNER_TEMP "hce-test-package-$Suffix"
$InstallDirectory = Join-Path $env:RUNNER_TEMP "hce-test-install-$Suffix"
$ArchiveName = "hce_windows_amd64.zip"
$Server = $null

try {
    New-Item -ItemType Directory -Path $ReleaseDirectory, $PackageDirectory -Force | Out-Null

    Push-Location $RepositoryRoot
    try {
        go build -trimpath -ldflags "-s -w -X main.version=$Version" -o (Join-Path $PackageDirectory "hce.exe") ./cmd/hce
        if ($LASTEXITCODE -ne 0) { throw "Windows installer test build failed" }
    }
    finally {
        Pop-Location
    }

    Copy-Item (Join-Path $RepositoryRoot "README.md") (Join-Path $PackageDirectory "README.md")
    Compress-Archive -Path (Join-Path $PackageDirectory "hce.exe"), (Join-Path $PackageDirectory "README.md") -DestinationPath (Join-Path $ReleaseDirectory $ArchiveName)

    $Hash = (Get-FileHash (Join-Path $ReleaseDirectory $ArchiveName) -Algorithm SHA256).Hash.ToLowerInvariant()
    "$Hash  $ArchiveName" | Set-Content (Join-Path $ReleaseDirectory "checksums.txt") -Encoding ascii

    $Server = Start-Process -FilePath "python" -ArgumentList @("-m", "http.server", "18765", "--bind", "127.0.0.1", "--directory", $ReleaseDirectory) -PassThru
    $ServerReady = $false
    for ($Attempt = 0; $Attempt -lt 20; $Attempt++) {
        try {
            Invoke-WebRequest "http://127.0.0.1:18765/checksums.txt" -UseBasicParsing | Out-Null
            $ServerReady = $true
            break
        }
        catch {
            Start-Sleep -Milliseconds 250
        }
    }
    if (-not $ServerReady) { throw "Local release server did not start" }

    $env:HCE_RELEASE_BASE = "http://127.0.0.1:18765"
    $env:HCE_INSTALL_DIR = $InstallDirectory
    & (Join-Path $RepositoryRoot "install.ps1")

    $InstalledExecutable = Join-Path $InstallDirectory "hce.exe"
    if (-not (Test-Path $InstalledExecutable)) { throw "Windows installer did not create hce.exe" }
    $InstalledVersion = (& $InstalledExecutable version).Trim()
    if ($InstalledVersion -ne $Version) {
        throw "Installed version was '$InstalledVersion'; expected '$Version'"
    }
    & $InstalledExecutable --help | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Installed hce.exe smoke test failed" }
}
finally {
    Remove-Item Env:HCE_RELEASE_BASE -ErrorAction SilentlyContinue
    Remove-Item Env:HCE_INSTALL_DIR -ErrorAction SilentlyContinue
    if ($Server -and -not $Server.HasExited) {
        Stop-Process -Id $Server.Id -Force
    }
    Remove-Item $ReleaseDirectory, $PackageDirectory, $InstallDirectory -Recurse -Force -ErrorAction SilentlyContinue
}
