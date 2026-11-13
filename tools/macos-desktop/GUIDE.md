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
run it again. "hostel screenshot" shows the whole desktop.

System dialogs (permission prompts, alerts of other processes) are not in
agent-device snapshots. Click them at screen coordinates:

  hostel screenshot -o screen.png       # pixels; the Mac screen is Retina
  hostel run -- 'agent-device click <x> <y> --platform macos'

x and y are points: the pixel position in the screenshot divided by 2 on a
Retina screen ("system_profiler SPDisplaysDataType" says "Retina"). The click
needs an agent-device session: run "agent-device open" for your app (or
com.apple.finder) first. Take a new screenshot to check the result. Do not
use osascript to control other apps: macOS asks for a permission that nobody
can answer.

Logs: "log show" and "log stream" need an administrator. Start the app's
executable in a job with OS_ACTIVITY_DT_MODE=YES to get its os_log lines on
stderr:

  hostel run --detach -- 'OS_ACTIVITY_DT_MODE=YES MyApp.app/Contents/MacOS/MyApp'

At the release, Hostel ends the apps and the launchd agents that the lease
started, and removes the apps and launch agents that it added to
~/Applications and ~/Library/LaunchAgents. Other files in the home directory
stay for the next lease, also of another user: app settings, keychain items,
"Always allow" answers, and browser profiles. Do not sign in to real accounts.
Start Chrome with a profile in the room, so that cleanup deletes it:

  hostel run -- 'open -na "Google Chrome" --args --user-data-dir="$HOSTEL_WORK_DIR/../chrome"'

A lease without macos-desktop does not get this cleanup: open apps only in a
macos-desktop lease.
