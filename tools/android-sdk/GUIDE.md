The Android SDK, for builds. Java comes from the jdk tool.

Environment: ANDROID_HOME; adb, sdkmanager, and emulator are on the PATH. The
SDK is on every lease of a machine that has it: ask for android-sdk only to
get such a machine. Ask for android-emulator only when you must see the app.

  hostel acquire android-sdk jdk --task "build"
  hostel run -- ./gradlew assembleDebug
