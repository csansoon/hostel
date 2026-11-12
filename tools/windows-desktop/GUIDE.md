The Windows desktop, for Windows apps. The lease takes the whole machine.
Commands run over SSH and cannot show windows; "desktop" runs a command on the
desktop, and winapp (Microsoft's winapp CLI) always runs there:

  hostel acquire windows-desktop --task "QA the editor"
  hostel run -- 'dotnet build -c Release -o out'
  hostel run -- 'desktop --detach out\MyApp.exe'
  hostel run -- 'winapp ui inspect -a MyApp --interactive'
  hostel run -- 'winapp ui invoke save -a MyApp'
  hostel run -- 'winapp ui set-value email "me@example.com" -a MyApp'
  hostel run -- 'winapp ui click save -a MyApp'
  hostel run -- 'winapp ui send-keys "text" -a MyApp --via send-input'

The machine facts list the build tools of the machine (for example the .NET
SDK). Prefer invoke and set-value (UI Automation). click and send-keys are real
input: they need the app in the foreground and fail if it is not. Start an app
in a normal window (not hidden or minimized). "hostel run -- winapp ui --help"
lists all commands. "hostel screenshot" shows the whole desktop.

Commands run in PowerShell. A cmdlet that fails does not change the exit
code; start a command with $ErrorActionPreference = 'Stop' when a failure must
stop it.

Packaged apps (MSIX): build and sign the package in a job, and install it in a
job (not through "desktop"):

  hostel run -- 'signtool sign /fd SHA256 /sm /sha1 <thumbprint> app.msix'
  hostel run -- '$ErrorActionPreference = "Stop"; Add-AppxPackage app.msix -DependencyPath (Get-ChildItem Dependencies\x64\*.msix)'

signtool works only if the machine has the certificate for Hostel
(docs/machine-setup.md, section 4.5). If another user of the machine has a
package with the same name and version, Add-AppxPackage fails with 0x80073D19:
give the package another name or version. A packaged app starts only on a
machine whose desktop user is the machine user; on a shared desktop, the start
fails with 0x80070520 ("A specified logon session does not exist").

At the release, Hostel ends the apps that "desktop" started and removes the
packages that the lease installed, with their app data.
