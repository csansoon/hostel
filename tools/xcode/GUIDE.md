Xcode, for builds of Apple apps. Ask for ios-simulator or macos-desktop only
when you must see the app.

  hostel acquire xcode --task "build"
  hostel run -- 'xcodebuild -scheme MyApp -destination "generic/platform=iOS Simulator" build'
