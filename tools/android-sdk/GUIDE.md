The Android SDK, for builds. Java comes from the jdk tool.

Environment: ANDROID_HOME; adb, sdkmanager, and emulator are on the PATH. The
SDK is on every lease of a machine that has it: ask for android-sdk only to
get such a machine. Ask for android-emulator only when you must see the app.

The SDK has the platforms that the version of this tool lists. A Gradle build
downloads the other platforms and build tools that it needs (the licenses are
accepted). They stay in the cache of the machine, like ~/.gradle, so the first
build on a machine is slow and the next ones are fast.

  hostel acquire android-sdk jdk --task "build"
  hostel run -- ./gradlew assembleDebug
