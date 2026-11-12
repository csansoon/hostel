# Removes the packages that the lease installed for the machine user, with
# their app data, so that no app session stays for the next lease. The desktop
# program has already ended the lease's desktop commands. A repeated stop (no
# record of start) does nothing.
$ErrorActionPreference = 'Stop'
$record = Join-Path $env:HOSTEL_ROOM_DIR 'desktop-packages'
if (-not (Test-Path $record)) { exit 0 }
$before = @(Get-Content $record)
$failed = @()
foreach ($p in Get-AppxPackage) {
	if ($before -contains $p.PackageFullName) { continue }
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
