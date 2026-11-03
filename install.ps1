# Installs Hostel on Windows from a GitHub release of csansoon/hostel.
#
#   irm https://raw.githubusercontent.com/csansoon/hostel/main/install.ps1 | iex
#
# It installs hostel.exe into $env:LOCALAPPDATA\Programs\hostel and adds that
# directory to the user PATH. It verifies the file against SHA256SUMS of the
# release. $env:HOSTEL_VERSION selects a release (for example v0.1.0; default:
# the latest); $env:HOSTEL_INSTALL_URL replaces the download URL (a mirror).
# The script block keeps its settings out of the caller's session (irm | iex).
& {
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'csansoon/hostel'
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$file = "hostel-windows-$arch.exe"
if ($env:HOSTEL_INSTALL_URL) {
	$base = $env:HOSTEL_INSTALL_URL
} elseif ($env:HOSTEL_VERSION) {
	$base = "https://github.com/$repo/releases/download/$($env:HOSTEL_VERSION)"
} else {
	$base = "https://github.com/$repo/releases/latest/download"
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("hostel-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
	Invoke-WebRequest -UseBasicParsing -TimeoutSec 300 -Uri "$base/SHA256SUMS" -OutFile "$tmp\SHA256SUMS"
	Invoke-WebRequest -UseBasicParsing -TimeoutSec 300 -Uri "$base/$file" -OutFile "$tmp\$file"
	$want = (Get-Content "$tmp\SHA256SUMS" | Where-Object { ($_ -split '\s+')[1] -eq $file } | ForEach-Object { ($_ -split '\s+')[0] })
	$have = (Get-FileHash -Algorithm SHA256 "$tmp\$file").Hash.ToLower()
	if (-not $want -or $have -ne $want) {
		throw "install.ps1: checksum mismatch for $file"
	}

	$dir = Join-Path $env:LOCALAPPDATA 'Programs\hostel'
	New-Item -ItemType Directory -Force -Path $dir | Out-Null
	Copy-Item -Force "$tmp\$file" "$dir\hostel.exe"

	# The user PATH as stored (REG_EXPAND_SZ, with %VARIABLES% unexpanded).
	$key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
	$path = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
	if (-not (($path -split ';') -contains $dir)) {
		$key.SetValue('Path', ($(if ($path) { "$path;" } else { '' }) + $dir), [Microsoft.Win32.RegistryValueKind]::ExpandString)
		# Tell running programs that the environment changed.
		[Environment]::SetEnvironmentVariable('HOSTEL_INSTALL_PATH_CHANGED', '1', 'User')
		[Environment]::SetEnvironmentVariable('HOSTEL_INSTALL_PATH_CHANGED', $null, 'User')
		$env:Path = "$env:Path;$dir"
		Write-Host "added $dir to your PATH (open a new terminal to use it)"
	}
	$key.Close()
	Write-Host "installed $dir\hostel.exe ($(& "$dir\hostel.exe" --version))"
} finally {
	Remove-Item -Recurse -Force $tmp
}
}
