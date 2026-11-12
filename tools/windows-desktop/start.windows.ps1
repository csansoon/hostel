# The desktop program must run: SSH sessions have no screen, and "desktop",
# winapp, and the screenshot use the program in the desktop session (the
# Hostel desktop task, docs/machine-setup.md, section 4.4).
$ErrorActionPreference = 'Stop'
$root = Split-Path $env:HOSTEL_CACHE_DIR
$file = Join-Path $root 'desktop.json'
$exe = Join-Path $root 'bin\hostel.exe'
# The program of the current helper: after a helper update, the old program
# removes its desktop.json within 2 s, and the desktop task starts the new one
# within 10 s. So the file must be newer than the helper, and its address must
# accept a connection.
function Test-Desktop {
	try {
		$f = Get-Item $file -ErrorAction Stop
		if ($f.LastWriteTimeUtc -lt (Get-Item $exe).LastWriteTimeUtc) { return $false }
		$addr = (Get-Content $file -Raw | ConvertFrom-Json).addr
		$i = $addr.LastIndexOf(':')
		$c = New-Object Net.Sockets.TcpClient
		try { return $c.ConnectAsync($addr.Substring(0, $i), [int]$addr.Substring($i + 1)).Wait(1000) } finally { $c.Dispose() }
	} catch { return $false }
}
$deadline = (Get-Date).AddSeconds(30)
while (-not (Test-Desktop) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
if (-not (Test-Desktop)) {
	[Console]::Error.WriteLine("the desktop program does not run: sign in the desktop user, and check the Hostel desktop task")
	exit 1
}
# Only a lease that asked for windows-desktop runs this, and that lease holds
# the whole machine. Stop removes what the lease added: the packages of the
# machine user (with their app data), and the processes of the desktop session.
$packages = @(Get-AppxPackage | ForEach-Object { $_.PackageFamilyName })
Set-Content -Encoding utf8 -Path (Join-Path $env:HOSTEL_ROOM_DIR 'desktop-packages') -Value $packages
# The desktop program: a hostel process of the machine user (Path is empty for
# the processes of other users) in a desktop session.
$program = @(Get-Process hostel | Where-Object { $_.SessionId -ne 0 -and $_.Path })
if (-not $program) {
	[Console]::Error.WriteLine("the desktop program does not run: sign in the desktop user, and check the Hostel desktop task")
	exit 1
}
$session = $program[0].SessionId
$procs = @(Get-Process | Where-Object SessionId -eq $session | ForEach-Object { "$($_.Id) $($_.StartTime.Ticks)" })
Set-Content -Encoding utf8 -Path (Join-Path $env:HOSTEL_ROOM_DIR 'desktop-processes') -Value (@("session $session") + $procs)
