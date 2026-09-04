# The Windows half of install-smoke.sh. Builds the release executables from
# this checkout, zips them the way goreleaser does, then runs install.ps1
# against that zip twice and refuses a bad checksum.
$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
$version = "0.0.0-smoke"
$builds = @(@{ Executable = "roast.exe"; Package = "./cmd/roast" })
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  "AMD64" { "amd64" }
  "ARM64" { "arm64" }
  default { throw "unsupported smoke architecture: $env:PROCESSOR_ARCHITECTURE" }
}
$work = Join-Path ([System.IO.Path]::GetTempPath()) "roast-install-smoke-$PID"

function Assert([bool]$Condition, [string]$Message) {
  if (-not $Condition) {
    throw "install smoke: $Message"
  }
}

function Invoke-Installer([string]$Dist, [string]$Bin) {
  $env:ROAST_INSTALL_BASE_URL = $Dist
  $env:ROAST_INSTALL_VERSION = $version
  $env:ROAST_INSTALL_DIR = $Bin
  $env:ROAST_INSTALL_SKIP_PATH = "1"
  # The installer's output goes to the host, not into this function's
  # return value, so the caller compares one exit code and not a list.
  & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root "install.ps1") | Out-Host
  return $LASTEXITCODE
}

try {
  New-Item -ItemType Directory -Force $work | Out-Null
  $dist = Join-Path $work "dist"
  $build = Join-Path $work "build"
  $bin = Join-Path $work "bin"
  New-Item -ItemType Directory -Force $dist, $build | Out-Null

  Push-Location $root
  try {
    foreach ($item in $builds) {
      & go build -buildvcs=false -ldflags "-s -w -X main.version=$version" -o (Join-Path $build $item.Executable) $item.Package
      Assert ($LASTEXITCODE -eq 0) "go build $($item.Package) failed"
    }
  } finally {
    Pop-Location
  }
  $archiveName = "roast_${version}_windows_${arch}.zip"
  $archive = Join-Path $dist $archiveName
  Compress-Archive -LiteralPath @($builds | ForEach-Object { Join-Path $build $_.Executable }) -DestinationPath $archive
  $hash = (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant()
  Set-Content -Path (Join-Path $dist "checksums.txt") -Value "$hash  $archiveName" -Encoding ascii

  Assert ((Invoke-Installer $dist $bin) -eq 0) "first install failed"
  Assert ((Invoke-Installer $dist $bin) -eq 0) "second install over the first failed"
  foreach ($item in $builds) {
    $installed = Join-Path $bin $item.Executable
    $reported = (& $installed --version | Out-String).Trim()
    $reportedVersion = [regex]::Match($reported, "v?(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)").Groups[1].Value
    Assert ($reportedVersion -eq $version) "installed $($item.Executable) reports '$reported', want '$version'"
  }

  $badDist = Join-Path $work "bad-dist"
  New-Item -ItemType Directory -Force $badDist | Out-Null
  Copy-Item $archive (Join-Path $badDist $archiveName)
  Set-Content -Path (Join-Path $badDist "checksums.txt") -Value ("0" * 64 + "  $archiveName") -Encoding ascii
  $badBin = Join-Path $work "bad-bin"
  Assert ((Invoke-Installer $badDist $badBin) -ne 0) "installer accepted a checksum mismatch"
  foreach ($item in $builds) {
    Assert (-not (Test-Path (Join-Path $badBin $item.Executable))) "checksum failure left $($item.Executable) behind"
  }
  Write-Output "install smoke passed"
} finally {
  Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
# The last native command above is the installer refusing the bad checksum,
# and Windows PowerShell would hand its exit code to whoever ran this script.
exit 0
