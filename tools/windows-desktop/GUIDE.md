The Windows desktop, for Windows apps. The lease takes the whole machine.
Commands run over SSH and cannot show windows; "desktop" runs a command on the
desktop, and winapp (Microsoft's winapp CLI) always runs there:

  hostel acquire dotnet windows-desktop --task "QA the editor"
  hostel run -- 'dotnet build -c Release -o out'
  hostel run -- 'desktop --detach out\MyApp.exe'
  hostel run -- 'winapp ui inspect -a MyApp --interactive'
  hostel run -- 'winapp ui invoke save -a MyApp'
  hostel run -- 'winapp ui set-value email "me@example.com" -a MyApp'
  hostel run -- 'winapp ui click save -a MyApp'
  hostel run -- 'winapp ui send-keys "text" -a MyApp --via send-input'

Prefer invoke and set-value (UI Automation). click and send-keys are real
input: they need the app in the foreground and fail if it is not. Start an app
in a normal window (not hidden or minimized). "hostel run -- winapp ui --help"
lists all commands. "hostel screenshot" shows the whole desktop. At the
release, Hostel ends the apps that "desktop" started.
