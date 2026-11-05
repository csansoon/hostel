iOS simulators: each lease gets new, clean simulators (one for each option, for
example iphone-17-pro or ipad-pro-13). Xcode comes with it.

Commands get SIMULATOR_UDID (the first option) and SIMULATOR_UDIDS (all,
comma-separated):

  hostel acquire ios-simulator:iphone-17-pro --task "QA login"
  hostel run -- 'xcodebuild ... -destination "id=$SIMULATOR_UDID" build'
  hostel run -- 'xcrun simctl install "$SIMULATOR_UDID" <path to the .app>'
  hostel run -- 'xcrun simctl launch "$SIMULATOR_UDID" <bundle id>'

To tap, type, and read the screen, use agent-device on the room. Open the app
on the lease's simulator once; later commands use that session:

  hostel run -- 'agent-device open <bundle id> --platform ios --udid "$SIMULATOR_UDID" --foreground'
  hostel run -- 'agent-device click "label=\"Sign in\"" --settle'
  hostel run -- 'agent-device fill "role=text-field" "me@example.com" --settle'

"hostel run -- agent-device help" lists all commands.
