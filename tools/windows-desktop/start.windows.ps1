# The desktop program must run: SSH sessions have no screen, and "desktop",
# winapp, and the screenshot use the program in the desktop session (the
# Hostel desktop task, docs/machine-setup.md, section 4.4).
$ErrorActionPreference = 'Stop'
$file = Join-Path (Split-Path $env:HOSTEL_CACHE_DIR) 'desktop.json'
# After a helper update, the desktop task starts the new program within 10 s.
$deadline = (Get-Date).AddSeconds(30)
while (-not (Test-Path $file) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
if (-not (Test-Path $file)) {
	[Console]::Error.WriteLine("the desktop program does not run: sign in the desktop user, and check the Hostel desktop task")
	exit 1
}
# Only a lease that asked for windows-desktop runs this, and that lease holds
# the whole machine. Stop removes what the lease added: the packages of the
# machine user (with their app data), and the processes of the desktop session.
$packages = @(Get-AppxPackage | ForEach-Object { $_.PackageFullName })
Set-Content -Encoding utf8 -Path (Join-Path $env:HOSTEL_ROOM_DIR 'desktop-packages') -Value $packages
$session = @(Get-Process hostel | Where-Object SessionId -ne 0)[0].SessionId
$procs = @(Get-Process | Where-Object SessionId -eq $session | ForEach-Object { "$($_.Id) $($_.StartTime.Ticks)" })
Set-Content -Encoding utf8 -Path (Join-Path $env:HOSTEL_ROOM_DIR 'desktop-processes') -Value (@("session $session") + $procs)
