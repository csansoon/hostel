# The desktop program must run: SSH sessions have no screen, and "desktop",
# winapp, and the screenshot use the program in the desktop session (the
# Hostel desktop task, docs/static-machine-setup.md, Windows, step 6).
$ErrorActionPreference = 'Stop'
$file = Join-Path (Split-Path $env:HOSTEL_CACHE_DIR) 'desktop.json'
if (-not (Test-Path $file)) {
	[Console]::Error.WriteLine("the desktop program does not run: sign in the desktop user, and check the Hostel desktop task")
	exit 1
}
