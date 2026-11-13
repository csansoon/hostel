# Ends the programs that the lease started in the desktop session, and removes
# the packages that it installed for the machine user, with their app data. The
# desktop program has already ended the lease's desktop commands; this finds
# the programs that the shell started for the lease (explorer, packaged apps),
# which do not have the lease in their environment. A repeated stop (no record
# of start) does nothing.
$ErrorActionPreference = 'Stop'
$record = Join-Path $env:HOSTEL_ROOM_DIR 'desktop-processes'
if (Test-Path $record) {
	$lines = @(Get-Content $record)
	$session = [int]($lines[0] -replace '^session ', '')
	if ($session -eq 0) { throw 'the record of start has no desktop session' }
	foreach ($p in Get-Process | Where-Object SessionId -eq $session) {
		# A process can end during the loop; then Path and StartTime throw.
		try { $path = $p.Path; $start = $p.StartTime.Ticks } catch { continue }
		# Path is empty for the processes of other users (Windows).
		if (-not $path -or $p.Name -in 'hostel', 'explorer') { continue }
		if ($lines -contains "$($p.Id) $start") { continue }
		# Windows starts its own per-user programs at any time. (An SSH session
		# cannot see the windows of the desktop session.)
		if ($path.StartsWith("$env:SystemRoot\", 'OrdinalIgnoreCase')) { continue }
		Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
	}
}
$record = Join-Path $env:HOSTEL_ROOM_DIR 'desktop-packages'
if (-not (Test-Path $record)) { exit 0 }
$before = @(Get-Content $record)
$failed = @()
# By family: an app that updated itself during the lease stays. Not frameworks
# (for example the Windows App Runtime that a package brought): Windows refuses
# to remove one, and removes it with the last package that needs it.
foreach ($p in Get-AppxPackage | Where-Object { -not $_.IsFramework }) {
	if ($before -contains $p.PackageFamilyName) { continue }
	try {
		Remove-AppxPackage -Package $p.PackageFullName
	} catch {
		$failed += "$($p.PackageFullName): $($_.Exception.Message)"
	}
}
if ($failed) {
	[Console]::Error.WriteLine("remove the packages of the lease: " + ($failed -join '; '))
	exit 1
}
