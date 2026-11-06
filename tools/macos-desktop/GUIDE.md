The macOS desktop, for Mac apps (also Mac Catalyst apps). The lease takes the
whole machine. Build the app, start it with "open", then control it:

  hostel acquire xcode macos-desktop --task "QA the Mac app"
  hostel run -- 'xcodebuild -scheme MyApp -derivedDataPath build build'
  hostel run -- 'open build/Build/Products/Debug/MyApp.app'
  hostel run -- 'agent-device open <bundle id> --platform macos'
  hostel run -- 'agent-device snapshot -i --platform macos'
  hostel run -- 'agent-device click "id=save" --platform macos'

The first agent-device command of a lease takes about 30 s. If a command says
that the runner is still finishing an earlier command, wait a few seconds and
run it again. "hostel screenshot" shows the whole desktop. At the release,
Hostel ends the apps that the lease started.
