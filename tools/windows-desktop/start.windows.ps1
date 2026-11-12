# The desktop program must run: SSH sessions have no screen, and "desktop",
# winapp, and the screenshot use the program in the desktop session (the
# Hostel desktop task, docs/machine-setup.md, section 4.4).
$ErrorActionPreference = 'Stop'
$file = Join-Path (Split-Path $env:HOSTEL_CACHE_DIR) 'desktop.json'
if (-not (Test-Path $file)) {
	[Console]::Error.WriteLine("the desktop program does not run: sign in the desktop user, and check the Hostel desktop task")
	exit 1
}
# The packages of the machine user, so that stop removes the packages that the
# lease installed (and their app data). Only a lease that asked for
# windows-desktop runs it, and that lease holds the whole machine.
$packages = @(Get-AppxPackage | ForEach-Object { $_.PackageFullName })
Set-Content -Encoding utf8 -Path (Join-Path $env:HOSTEL_ROOM_DIR 'desktop-packages') -Value $packages
