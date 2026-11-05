# Static room setup: a Mac for iOS

This guide prepares a Mac as a static Hostel room (milestone M2: the spare MacBook).
The owner does these steps once.

Two machines take part:

- **The server Mac**: the owner's own Mac. The Hostel server runs on it.
- **The room Mac**: the spare MacBook.

Do not install Hostel on the room Mac. The server connects to it over SSH and
installs everything itself (the helper binary, mise, the tool scripts, and the
simulators). Except where a step says "on the server Mac", do the steps on the room
Mac.

In M2, the server reaches the room Mac over the local network. A room behind NAT
that a remote server uses needs `hostel room connect` (M5).

Since M9, `hostel machine join` does the user, the server key, and the
connection service for you (`deploy-vps.md`, section 6). The sections below
describe the same steps by hand, and the steps that only a person can do
(Remote Login, Xcode, the desktop session).

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
   developer.apple.com). The `xcode` tool reports this version (its `check`
   script); a guest can ask for `xcode:26`.
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

Do not create simulators. The `ios-simulator` tool scripts create and delete
their own simulators.

## 6. Desktop session

The `macos-desktop` tool takes desktop screenshots, and the iOS Simulator app needs a
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
5. Optional, for the `macos-desktop` tool (not needed for `ios-simulator`): let UI
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

Then list the tools of the machine in the configuration (`hostel config edit`):

```toml
tools = ["xcode", "ios-simulator", "macos-desktop"]   # add "android-emulator" and "jdk:21" for Android
```

On the server Mac:

```sh
ssh -i ~/.local/share/hostel/server_ed25519 hostel@<address> 'uname -sm; sw_vers -productVersion; xcodebuild -version; df -h ~'
```

Send to the agent: the address (`hostel@<address>`), and the output of this
command.

## Windows (M6)

A Windows machine is a static room for command-line work: builds and tests
over SSH. Desktop UI needs the `windows-desktop` tool (step 6). The reference room is the
owner's Cloud PC `win-pc`. Do these steps as an administrator, in PowerShell.

1. **OpenSSH Server.** It must run and start automatically
   (`Get-Service sshd`). Its default shell can stay as it is (PowerShell or
   `cmd`); the server finds it at the probe.
2. **The room user.** Create a local user without administrator rights, with a
   random password that nobody keeps (the server logs in with its key only):

   ```powershell
   $b = New-Object byte[] 32; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
   $pw = ConvertTo-SecureString ([Convert]::ToBase64String($b) + 'aA1!') -AsPlainText -Force
   New-LocalUser -Name hostel -Password $pw -PasswordNeverExpires -AccountNeverExpires
   Add-LocalGroupMember -Group Users -Member hostel
   # The profile must exist before the first SSH login.
   $cred = New-Object Management.Automation.PSCredential('hostel', $pw)
   Start-Process cmd.exe -ArgumentList '/c','exit' -Credential $cred -LoadUserProfile -WorkingDirectory C:\Windows -Wait
   ```

3. **The server key.** Put the output of `hostel admin pubkey` into
   `C:\Users\hostel\.ssh\authorized_keys`. The SSH server accepts the file only
   if SYSTEM and the user are the only users with access:

   ```powershell
   icacls C:\Users\hostel\.ssh\authorized_keys /inheritance:r /grant 'SYSTEM:F' /grant 'hostel:F'
   ```

   After the first lease, cleanup restores the file from
   `C:\Users\hostel\hostel\authorized_keys.base`; a new server key goes into both
   files.
4. **Configuration** on the server:

   ```toml
   [providers.home.machines.win]
   address = "hostel@localhost"   # with connect = true, only the user is used
   os = "windows"
   connect = true
   tools = ["dotnet:9"]           # mise tools; add "windows-desktop" after step 6
   ```

5. **`room connect`** as a scheduled task of `LOCAL SERVICE`:
   `deploy-vps.md`, section 6.
6. **Desktop (M7, optional).** For desktop leases (the tool
   `windows-desktop`), a user must be signed in to the desktop, and the desktop must
   stay connected and unlocked. SSH sessions cannot show windows, take
   screenshots, or use UI Automation. So a scheduled task of the desktop user
   starts `hostel room desktop serve` as `hostel` in the desktop session, and
   lease commands go to it (`desktop`, `winapp`). The apps of a lease run as
   `hostel`, not as the desktop user. Run this as an administrator while the
   desktop user is signed in:

   ```powershell
   $dir = 'C:\ProgramData\hostel\desktop'
   $user = (Get-Process explorer -IncludeUserName | Select-Object -First 1).UserName
   New-Item -ItemType Directory -Force $dir | Out-Null
   # A new random password for hostel, readable only by the desktop user's task.
   $b = New-Object byte[] 32; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
   $pw = [Convert]::ToBase64String($b) + 'aA1!'
   Set-LocalUser -Name hostel -Password (ConvertTo-SecureString $pw -AsPlainText -Force)
   Set-Content "$dir\cred" $pw -NoNewline
   icacls "$dir\cred" /inheritance:r /grant:r 'SYSTEM:F' 'Administrators:F' "${user}:R" | Out-Null
   icacls $dir /grant "${user}:(OI)(CI)RX" | Out-Null
   Set-Content "$dir\start.ps1" @'
   # Keeps "hostel room desktop serve" running as hostel on this desktop. It
   # ends when the helper changes; then this starts the new one.
   $dir = 'C:\ProgramData\hostel\desktop'
   while ($true) {
     try {
       $pw = ConvertTo-SecureString (Get-Content "$dir\cred" -Raw).Trim() -AsPlainText -Force
       $cred = New-Object Management.Automation.PSCredential("$env:COMPUTERNAME\hostel", $pw)
       $p = Start-Process C:\Users\hostel\hostel\bin\hostel.exe -ArgumentList 'room','desktop','serve' -Credential $cred -LoadUserProfile -WorkingDirectory C:\Users\hostel -WindowStyle Hidden -PassThru
       $p.WaitForExit()
     } catch {}
     Start-Sleep 10
   }
   '@
   $action = New-ScheduledTaskAction -Execute 'conhost.exe' -Argument "--headless powershell.exe -NoProfile -ExecutionPolicy Bypass -File $dir\start.ps1"
   $trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
   $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
   $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit 0 -MultipleInstances IgnoreNew -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
   Register-ScheduledTask -TaskName 'Hostel desktop' -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
   Start-ScheduledTask -TaskName 'Hostel desktop'
   ```

   Add `windows-desktop` to the machine's `tools`. The `hostel` user cannot read
   `cred`. Until the server installs the helper (the first provisioning), the
   task tries again every 10 seconds. A lease can send keys and clicks to any
   window on this desktop that is not elevated, also to the desktop user's
   windows: use a desktop user that has nothing else open, or accept this.

Notes:

- Guest commands run with PowerShell 7 if it is in the `PATH`, else Windows
  PowerShell 5.1. The setup job installs the repository's mise tools.
- On Windows, a background process of a job ends when the job's command ends.
  Use `hostel run --detach` for a long-running server.
- Microsoft Defender did not block the helper on `win-pc`. If it does, add an
  exclusion for `C:\Users\hostel\hostel`.
