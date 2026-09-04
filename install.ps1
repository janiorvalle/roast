# The whole installer runs in its own scope, so `irm ... | iex` in a terminal
# leaves that terminal's preferences, functions, and variables as they were.
& {
$ErrorActionPreference = "Stop"

function Fail([string]$Message) {
  throw "roast installer: $Message"
}

function Publish-EnvironmentChange {
  if (-not ([System.Management.Automation.PSTypeName]"Roast.EnvironmentChange").Type) {
    Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;

namespace Roast {
  public static class EnvironmentChange {
    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern IntPtr SendMessageTimeout(
      IntPtr window,
      uint message,
      UIntPtr messageParameter,
      string environmentName,
      uint flags,
      uint timeout,
      out UIntPtr result
    );
  }
}
"@
  }

  $result = [UIntPtr]::Zero
  [void][Roast.EnvironmentChange]::SendMessageTimeout(
    [IntPtr]0xffff,
    [uint32]0x001A,
    [UIntPtr]::Zero,
    "Environment",
    [uint32]0x0002,
    [uint32]5000,
    [ref]$result
  )
}

$repo = if ($env:ROAST_INSTALL_REPO) { $env:ROAST_INSTALL_REPO } else { "janiorvalle/roast" }
# The folder goes into the user PATH, so a relative override is made absolute
# against the current location first.
$installDir = if ($env:ROAST_INSTALL_DIR) {
  $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($env:ROAST_INSTALL_DIR)
} else {
  Join-Path $env:LOCALAPPDATA "Programs\roast"
}
$version = $env:ROAST_INSTALL_VERSION
$executables = @("roast.exe")
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  "AMD64" { "amd64" }
  "ARM64" { "arm64" }
  default { Fail "unsupported architecture: $env:PROCESSOR_ARCHITECTURE; roast ships for amd64 and arm64" }
}

# A token lifts the GitHub API's limit of sixty unauthenticated requests an
# hour per address, which shared machines such as CI runners hit.
$githubToken = if ($env:ROAST_GITHUB_TOKEN) {
  $env:ROAST_GITHUB_TOKEN
} elseif ($env:GH_TOKEN) {
  $env:GH_TOKEN
} else {
  $env:GITHUB_TOKEN
}
$apiHeaders = @{
  Accept = "application/vnd.github+json"
  "User-Agent" = "roast-installer"
}
if ($githubToken) {
  $apiHeaders.Authorization = "Bearer $githubToken"
}

if (-not $version) {
  try {
    $release = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest" -Headers $apiHeaders -ErrorAction Stop
  } catch {
    $status = if ($_.Exception.Response) { [int]$_.Exception.Response.StatusCode } else { 0 }
    if ($status -eq 403 -or $status -eq 429) {
      Fail "GitHub answered HTTP $status to the latest-release lookup for $repo, its rate limit for requests without a token. Set GH_TOKEN or ROAST_GITHUB_TOKEN to a GitHub token, or set ROAST_INSTALL_VERSION to skip the lookup, then retry."
    }
    if ($status -eq 0) {
      Fail "could not reach api.github.com for the latest release of $repo. Check the network, or set ROAST_INSTALL_VERSION to skip the lookup, then retry."
    }
    Fail "GitHub answered HTTP $status to the latest-release lookup for $repo; see https://github.com/$repo/releases"
  }
  $version = [string]$release.tag_name
}
$version = $version.TrimStart("v")
$archiveName = if ($env:ROAST_INSTALL_ARCHIVE) {
  $env:ROAST_INSTALL_ARCHIVE
} else {
  "roast_${version}_windows_${arch}.zip"
}
# Same as install.sh: a mirror's base URL, or a local folder holding the
# archive and the checksums file, which is how the install smoke runs.
$baseUrl = if ($env:ROAST_INSTALL_BASE_URL) {
  $env:ROAST_INSTALL_BASE_URL.TrimEnd("/")
} else {
  "https://github.com/$repo/releases/download/v$version"
}

$temporaryDirectory = Join-Path ([System.IO.Path]::GetTempPath()) "roast-install-$PID"
New-Item -ItemType Directory -Force $temporaryDirectory | Out-Null
$archive = Join-Path $temporaryDirectory $archiveName
$checksums = Join-Path $temporaryDirectory "checksums.txt"
$extracted = Join-Path $temporaryDirectory "extracted"

function Fetch([string]$Name, [string]$Destination) {
  if (Test-Path -LiteralPath $baseUrl -PathType Container) {
    $local = Join-Path $baseUrl $Name
    if (-not (Test-Path -LiteralPath $local)) {
      Fail "$baseUrl has no $Name"
    }
    Copy-Item -LiteralPath $local $Destination
    return
  }
  try {
    Invoke-WebRequest "$baseUrl/$Name" -OutFile $Destination -UseBasicParsing -ErrorAction Stop
  } catch {
    Fail "could not download $baseUrl/$Name"
  }
}

# Reported-Version is the version number inside what --version prints, so
# "roast 1.2.3" and a bare "1.2.3" both compare as "1.2.3".
function Reported-Version([string]$Output) {
  return [regex]::Match($Output, "v?(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)").Groups[1].Value
}

function Assert-Version([string]$Executable, [string]$Stage) {
  $name = [System.IO.Path]::GetFileName($Executable)
  $reported = (& $Executable --version | Out-String).Trim()
  if ($LASTEXITCODE -ne 0) {
    Fail "$Stage $name failed its version smoke test (exit $LASTEXITCODE): $reported. Confirm Windows security tools allow the release executable, then retry the installer."
  }
  if ((Reported-Version $reported) -ne $version) {
    Fail "$Stage $name reported '$reported' instead of '$version'. Retry the installer; if it still fails, report both versions."
  }
}

try {
  Write-Output "Fetching roast $version for windows/$arch from $baseUrl..."
  Fetch $archiveName $archive
  Fetch "checksums.txt" $checksums

  $escapedArchiveName = [regex]::Escape($archiveName)
  $checksumLine = Get-Content $checksums |
    Where-Object { $_ -match "^[0-9a-fA-F]{64}\s+\*?$escapedArchiveName$" } |
    Select-Object -First 1
  if (-not $checksumLine) {
    Fail "checksums.txt has no entry for $archiveName"
  }
  $expected = ($checksumLine -split "\s+")[0].ToLowerInvariant()
  $actual = (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant()
  if ($actual -ne $expected) {
    Fail "checksum mismatch for $archiveName"
  }

  Expand-Archive -LiteralPath $archive -DestinationPath $extracted -Force
  $installs = @(foreach ($executable in $executables) {
    $downloaded = Get-ChildItem -LiteralPath $extracted -Recurse -Filter $executable | Select-Object -First 1
    if (-not $downloaded) {
      Fail "archive did not contain $executable"
    }
    Assert-Version $downloaded.FullName "downloaded"
    $destination = Join-Path $installDir $executable
    @{
      Name = $executable
      Downloaded = $downloaded.FullName
      Destination = $destination
      Stage = Join-Path $installDir ".$executable.new.$PID"
      Previous = Join-Path $temporaryDirectory ".$executable.previous"
      HadPrevious = Test-Path -LiteralPath $destination
      Replaced = $false
    }
  })

  New-Item -ItemType Directory -Force $installDir | Out-Null
  try {
    foreach ($install in $installs) {
      Copy-Item $install.Downloaded $install.Stage
      if ($install.HadPrevious) {
        Copy-Item $install.Destination $install.Previous
        Remove-Item -Force $install.Destination
      }
    }
    foreach ($install in $installs) {
      Move-Item -Force $install.Stage $install.Destination
      $install.Replaced = $true
      Assert-Version $install.Destination "installed"
    }
  } catch {
    # Whatever failed, the folder ends up as it was: the staged and the
    # unusable new files go, the previous binaries come back. A destination
    # that was never replaced is still the previous binary and stays.
    foreach ($install in $installs) {
      Remove-Item -Force $install.Stage -ErrorAction SilentlyContinue
      if ($install.Replaced) {
        Remove-Item -Force $install.Destination -ErrorAction SilentlyContinue
      }
      if ($install.HadPrevious -and -not (Test-Path -LiteralPath $install.Destination)) {
        Copy-Item $install.Previous $install.Destination -ErrorAction SilentlyContinue
      }
    }
    throw
  }

  Write-Output "Installed roast $version to $installDir"
  if (-not $env:ROAST_INSTALL_SKIP_PATH) {
    $environmentKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey("Environment", $true)
    if (-not $environmentKey) {
      Fail "could not open the user environment registry key"
    }
    try {
      $rawUserPath = [string]$environmentKey.GetValue(
        "Path",
        "",
        [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
      )
      $expandedUserPath = [string]$environmentKey.GetValue("Path", "")
      $expandedEntries = @($expandedUserPath -split ";" | Where-Object { $_ })
      if ($installDir -notin $expandedEntries) {
        $rawEntries = @($rawUserPath -split ";" | Where-Object { $_ })
        $updatedPath = (@($rawEntries) + $installDir) -join ";"
        $pathKind = [Microsoft.Win32.RegistryValueKind]::ExpandString
        if ($environmentKey.GetValueNames() -contains "Path") {
          $pathKind = $environmentKey.GetValueKind("Path")
        }
        if (
          $pathKind -ne [Microsoft.Win32.RegistryValueKind]::String -and
          $pathKind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString
        ) {
          Fail "the user PATH registry value is not a string"
        }
        $environmentKey.SetValue("Path", $updatedPath, $pathKind)
        Publish-EnvironmentChange
        Write-Output "Added $installDir to your user PATH. Open a new terminal before running roast."
      }
    } finally {
      $environmentKey.Dispose()
    }
  }
} finally {
  Remove-Item -Recurse -Force $temporaryDirectory -ErrorAction SilentlyContinue
}
}
