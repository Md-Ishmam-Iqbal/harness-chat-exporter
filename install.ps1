$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repository = "Md-Ishmam-Iqbal/harness-chat-exporter"
$ReleaseBase = if ($env:HCE_RELEASE_BASE) { $env:HCE_RELEASE_BASE } else { "https://github.com/$Repository/releases/latest/download" }
$Architecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$Architecture = $Architecture.ToLowerInvariant()
switch ($Architecture) {
    "amd64" { $Architecture = "amd64" }
    "arm64" { $Architecture = "arm64" }
    default { throw "hce install: unsupported CPU architecture: $Architecture" }
}

$Archive = "hce_windows_$Architecture.zip"
$TemporaryDirectory = Join-Path $env:TEMP ("hce-install-" + [guid]::NewGuid().ToString("N"))
$InstallDirectory = if ($env:HCE_INSTALL_DIR) { $env:HCE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\hce" }

try {
    New-Item -ItemType Directory -Path $TemporaryDirectory -Force | Out-Null
    Write-Host "Downloading hce for windows/$Architecture..."
    Invoke-WebRequest "$ReleaseBase/$Archive" -OutFile (Join-Path $TemporaryDirectory $Archive) -UseBasicParsing
    Invoke-WebRequest "$ReleaseBase/checksums.txt" -OutFile (Join-Path $TemporaryDirectory "checksums.txt") -UseBasicParsing

    $ChecksumLine = Get-Content (Join-Path $TemporaryDirectory "checksums.txt") |
        Where-Object { ($_ -split "\s+")[-1] -eq $Archive } |
        Select-Object -First 1
    if (-not $ChecksumLine) { throw "hce install: release checksum is missing for $Archive" }
    $Expected = ($ChecksumLine -split "\s+")[0].ToLowerInvariant()
    $Actual = (Get-FileHash (Join-Path $TemporaryDirectory $Archive) -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($Actual -ne $Expected) { throw "hce install: download checksum did not match" }

    $Expanded = Join-Path $TemporaryDirectory "expanded"
    Expand-Archive (Join-Path $TemporaryDirectory $Archive) -DestinationPath $Expanded -Force
    New-Item -ItemType Directory -Path $InstallDirectory -Force | Out-Null
    Copy-Item (Join-Path $Expanded "hce.exe") (Join-Path $InstallDirectory "hce.exe") -Force
    Unblock-File (Join-Path $InstallDirectory "hce.exe")

    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $PathEntries = @($UserPath -split ";" | Where-Object { $_ })
    if ($PathEntries -notcontains $InstallDirectory) {
        $NewUserPath = (@($PathEntries) + $InstallDirectory) -join ";"
        [Environment]::SetEnvironmentVariable("Path", $NewUserPath, "User")
    }
    if (($env:Path -split ";") -notcontains $InstallDirectory) {
        $env:Path = "$InstallDirectory;$env:Path"
    }

    Write-Host "Installed $(Join-Path $InstallDirectory 'hce.exe')"
    & (Join-Path $InstallDirectory "hce.exe") version
    Write-Host "`nRun: hce"
}
finally {
    if (Test-Path $TemporaryDirectory) {
        Remove-Item $TemporaryDirectory -Recurse -Force
    }
}
