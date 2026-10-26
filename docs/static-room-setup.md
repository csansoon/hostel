# Static room setup: a Mac for iOS

This guide prepares a Mac as a static Hostel room (milestone M2: the spare MacBook).
The owner does these steps once.

Two machines take part:

- **The server Mac**: the owner's own Mac. The Hostel server runs on it.
- **The room Mac**: the spare MacBook.

Do not install Hostel on the room Mac. The server connects to it over SSH and
installs everything itself (the helper binary, mise, the platform scripts, and the
simulators). Except where a step says "on the server Mac", do the steps on the room
Mac.

In M2, the server reaches the room Mac over the local network. A room behind NAT
that a remote server uses needs `hostel room connect` (M5).

## 1. Requirements

- Apple silicon. macOS 26.
- At least 100 GB of free disk: Xcode (about 15 GB), the iOS runtime (about 8 GB),
  DerivedData, mise tools, and package caches.
- The MacBook stays on power, with the lid open. With the lid closed, a MacBook
  without an external display goes to sleep.
- A fixed address on the local network: a DHCP reservation for its IP address, or
  the Bonjour name (`<name>.local`).

## 2. The `hostel` user

Hostel uses one macOS user for all leases. It is a standard user, not an
administrator.

1. As an administrator, create the user:

   ```sh
   sudo sysadminctl -addUser hostel -fullName "Hostel" -password -
   ```

   Type a strong password. Hostel does not use it; keep it for maintenance.

2. Let the user run the developer tools (simulators, debugging) without an
   administrator password prompt. A prompt over SSH stops a script until its
   timeout.

   ```sh
   sudo DevToolsSecurity -enable
   sudo dseditgroup -o edit -a hostel -t user _developer
   ```

## 3. Remote Login

1. System Settings > General > Sharing > Remote Login: on.
2. Click (i) next to Remote Login. Set "Allow access for" to "Only these users",
   and add `hostel`. Add your own user too if you need it.
3. Turn off password login for SSH. Add this file as an administrator:

   ```sh
   printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' | sudo tee /etc/ssh/sshd_config.d/100-hostel.conf
   sudo sshd -t && echo "sshd config OK"
   sudo launchctl kickstart -k system/com.openssh.sshd
   ```

   This applies to all users. Before you do it, make sure that your own key works
   for your user, or that you do not need SSH for it.

## 4. The server key

1. On the server Mac, print the server's public key:

   ```sh
   hostel admin pubkey
   ```

2. On the room Mac, add it for the `hostel` user:

   ```sh
   sudo -u hostel mkdir -p -m 700 /Users/hostel/.ssh
   echo '<the public key line>' | sudo -u hostel tee -a /Users/hostel/.ssh/authorized_keys
   sudo -u hostel chmod 600 /Users/hostel/.ssh/authorized_keys
   ```

   Add only the server key. Hostel saves this file at the first lease and restores
   it at each cleanup, so a key that a guest adds does not stay.

## 5. Xcode and the iOS runtime

As an administrator:

1. Install Xcode 26.5 to `/Applications/Xcode.app` (App Store or
   developer.apple.com). The machine label `xcode-26.5` states this version.
2. Select it, accept the license, and install the extra components:

   ```sh
   sudo xcode-select -s /Applications/Xcode.app
   sudo xcodebuild -license accept
   sudo xcodebuild -runFirstLaunch
   ```

3. Install the iOS 26.5 simulator runtime:

   ```sh
   xcodebuild -downloadPlatform iOS
   ```

4. Check as the `hostel` user:

   ```sh
   sudo -u hostel xcrun simctl list runtimes
   ```

   The list must have "iOS 26.5".

Do not create simulators. The `ios` platform scripts create and delete their own
simulators.

## 6. Desktop session

The `macos` platform takes desktop screenshots, and the iOS Simulator app needs a
desktop session to show a window. For this, the `hostel` user must be logged in at
the console after each restart.

1. System Settings > Users & Groups > "Automatically log in as": `hostel`.
   macOS allows this only when FileVault is off.
2. System Settings > Lock Screen: set "Start Screen Saver when inactive" and "Turn
   display off when inactive" to "Never", and "Require password after screen saver
   begins or display is turned off" to "Never" for the `hostel` user. A locked
   screen gives a screenshot of the lock screen.
3. Do not let the room Mac sleep:

   ```sh
   sudo pmset -a sleep 0 disksleep 0
   ```

4. Screen Recording permission for screenshots over SSH: System Settings >
   Privacy & Security > Screen & System Audio Recording. Click "+", press
   Cmd-Shift-G, type `/usr/libexec/sshd-keygen-wrapper`, and add it. M2 verified
   this entry on 2026-10-22.
5. Optional, for future macOS desktop support (not needed for iOS): let UI
   automation of desktop apps run without a password prompt. `agent-device`
   drives macOS apps with XCTest, and macOS asks for an administrator before it
   allows this (Automation Mode). As an administrator:

   ```sh
   sudo automationmodetool enable-automationmode-without-authentication
   automationmodetool
   ```

   The second command must not say "requires user authentication". The iOS
   Simulator does not need this step.

Security note: with FileVault off and automatic login, a person with physical
access to the room Mac can read its disk and use the `hostel` session. Use this only
on a spare machine with no personal data. Step 5 also lets any process on the room
Mac control its desktop through UI automation, without a password.

## 7. Check and send the details

On the server Mac:

```sh
ssh -i ~/.local/share/hostel/server_ed25519 hostel@<address> 'uname -sm; sw_vers -productVersion; xcodebuild -version; df -h ~'
```

Send to the agent: the address (`hostel@<address>`), and the output of this
command.
