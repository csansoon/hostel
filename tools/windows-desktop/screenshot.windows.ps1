# $args[0] is the output path. SSH sessions have no screen, so the capture runs
# in the desktop session through "desktop" (the Hostel desktop task,
# docs/machine-setup.md, section 4.4).
param([string] $Out, [switch] $Capture)
$ErrorActionPreference = 'Stop'
if (-not $Capture) {
	desktop powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $PSCommandPath $Out -Capture
	exit $LASTEXITCODE
}
# Without DPI awareness, the capture is scaled down to the 100% resolution.
Add-Type 'using System.Runtime.InteropServices; public static class HostelDpi { [DllImport("user32.dll")] public static extern bool SetProcessDPIAware(); }'
[HostelDpi]::SetProcessDPIAware() | Out-Null
Add-Type -AssemblyName System.Windows.Forms, System.Drawing
$screen = [System.Windows.Forms.SystemInformation]::VirtualScreen
$bitmap = New-Object System.Drawing.Bitmap $screen.Width, $screen.Height
$graphics = [System.Drawing.Graphics]::FromImage($bitmap)
try {
	$graphics.CopyFromScreen($screen.Left, $screen.Top, 0, 0, $bitmap.Size)
} catch {
	[Console]::Error.WriteLine("the screen capture failed ($_): the desktop must be connected and unlocked")
	exit 1
}
$bitmap.Save($Out, [System.Drawing.Imaging.ImageFormat]::Png)
$graphics.Dispose()
$bitmap.Dispose()
