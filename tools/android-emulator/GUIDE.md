Android emulators: each lease gets new, clean emulators (one for each option,
for example pixel-9 or pixel-tablet). The SDK comes with it.

Commands get ANDROID_SERIAL (the first option) and ANDROID_SERIALS (all,
comma-separated):

  hostel acquire android-emulator:pixel-9 --task "QA settings"
  hostel run -- ./gradlew installDebug
  hostel run -- 'adb -s "$ANDROID_SERIAL" shell am start -n <package>/<activity>'

To tap, type, and read the screen, use agent-device on the room with
--platform android; "hostel run -- agent-device help" lists all commands.

"hostel screenshot" shows the first emulator; "hostel screenshot --tool
android-emulator:<option>" another one.
