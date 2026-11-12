# Machine setup

This guide adds a computer to Hostel as a machine: a Mac, a Windows computer,
or a Linux computer. An administrator of that computer does these steps once.
For on-demand machines from a provider (Namespace), see section 7.

Each part ends with a check. Do not continue before the check passes.

## 1. Overview

`hostel machine join` does most of the work. A person does the rest:

| Hostel does it (`machine join`, then each lease) | Only a person can do it |
|---|---|
| Creates the user `hostel` (not an administrator) | Remote Login or OpenSSH Server |
| Installs the server's SSH key for `hostel` | Xcode, the .NET SDK, and other large installs |
| Installs the service that keeps the machine connected | Automatic login and no screen lock (desktop tools) |
| Sends the helper, mise, and the tool scripts | Permissions (macOS privacy settings), Automation Mode |
| Installs the tools of the machine (`install` scripts) and checks them | Apps that leases use but cannot install (for example Chrome) |
| Cleans up after each lease | Signing material (optional, section 3.7 and 4.5) |

What each tool needs on the machine:

| Tool | OS | Person's steps |
|---|---|---|
| `xcode` | macOS | Xcode (3.3) |
| `ios-simulator` | macOS | Xcode and the iOS runtime (3.3), a desktop session (3.4) |
| `macos-desktop` | macOS | A desktop session (3.4), Screen Recording and Automation Mode (3.5); for app QA, 3.6 |
| `android-sdk`, `android-emulator` | macOS, Linux | Nothing on macOS; KVM on Linux (5) |
| `windows-desktop` | Windows | A signed-in desktop and the desktop task (4.4) |
| mise tools (`jdk:21`, `node:22`, `dotnet:10`) | all | Nothing |

Hostel uses one OS user for all leases. Files outside the room directory (caches,
app settings, keychains, browser profiles) stay for the next lease, also of
another user. Use a computer with no personal data, and use accounts and
certificates only for Hostel.

## 2. All machines

1. **Power.** The computer stays on and does not sleep. A MacBook stays on power
   with the lid open.
2. **Network.** The computer reaches the Hostel server over HTTPS. It needs no
   inbound connection.
3. **Disk.** Free space for the tools and their caches. Typical sizes: Xcode
   about 15 GB, the iOS runtime about 8 GB, the Android SDK and system images
   about 6 GB, the .NET SDK and NuGet packages about 5 GB, plus build output.
   Keep at least 30 GB free after the installs.
4. **An administrator account** that is not `hostel`. You use it for this setup
   and for updates.
5. **Make the machine code.** On your own computer, connected as a Hostel admin:

   ```sh
   hostel config machines add <name> --tool <tool> [--tool <tool> ...]
   ```

   It prints one command for macOS and Linux and one for Windows. The code works
   once, for 24 hours. In the Hostel app: Rooms, then `+`.

Check, after the OS part: `hostel status` shows the machine as `free` within a
few minutes, and `hostel tools` lists it for its tools.

## 3. Mac

Apple silicon and macOS 26 or later.

### 3.1 Join

As the administrator, run the printed command (`curl … | sudo sh -s -- machine
join <code>`). It creates `hostel`, installs the server key, and installs the
LaunchDaemon `so.hostel.machine-connect` (log:
`/var/log/hostel-machine-connect.log`). The token is in `/etc/hostel`, which
`hostel` cannot read. If a step fails, fix it and run the same command again.

Let `hostel` use the developer tools without a password prompt (a prompt over
SSH stops a script until its timeout):

```sh
sudo DevToolsSecurity -enable
sudo dseditgroup -o edit -a hostel -t user _developer
```

### 3.2 Remote Login

1. System Settings > General > Sharing > Remote Login: on.
2. Click (i) next to Remote Login. "Allow access for": only these users, with
   `hostel` (and your own user if you need it).
3. Optional: turn off password login for SSH:

   ```sh
   printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' | sudo tee /etc/ssh/sshd_config.d/100-hostel.conf
   sudo sshd -t && sudo launchctl kickstart -k system/com.openssh.sshd
   ```

Check: `/var/log/hostel-machine-connect.log` says that the machine is connected.

### 3.3 Xcode and the iOS runtime

For `xcode` and `ios-simulator`:

```sh
# Install Xcode in /Applications/Xcode.app (App Store or developer.apple.com), then:
sudo xcode-select -s /Applications/Xcode.app
sudo xcodebuild -license accept
sudo xcodebuild -runFirstLaunch
xcodebuild -downloadPlatform iOS
sudo -u hostel xcrun simctl list runtimes      # must list the iOS runtime
```

Do not create simulators: `ios-simulator` creates and deletes its own. For more
than one Xcode, set `DEVELOPER_DIR` in the machine's `env`.

### 3.4 Desktop session

For `ios-simulator` and `macos-desktop`. The `hostel` user must be logged in at
the console, with the screen unlocked, after each restart.

1. System Settings > Users & Groups > "Automatically log in as": `hostel`. This
   needs FileVault off.
2. As `hostel`, in System Settings > Lock Screen: "Start Screen Saver when
   inactive" and "Turn display off when inactive": Never. "Require password
   after screen saver begins or display is turned off": Never.
3. `sudo pmset -a sleep 0 disksleep 0`
4. Restart. The desktop of `hostel` must show without a password.

Security note: with FileVault off and automatic login, a person with physical
access can read the disk and use the session. Use a spare computer.

### 3.5 Screenshots and UI control

For `macos-desktop` (and screenshots of `ios-simulator`):

1. **Screen Recording** for screenshots over SSH: System Settings > Privacy &
   Security > Screen & System Audio Recording > "+", press Cmd-Shift-G, type
   `/usr/libexec/sshd-keygen-wrapper`, and add it.
2. **Automation Mode** without a password, for agent-device (XCTest):

   ```sh
   sudo automationmodetool enable-automationmode-without-authentication
   automationmodetool        # must not say "requires user authentication"
   ```

   Any process on the Mac can then control its desktop.

Check: a lease with `macos-desktop` gives a screenshot of the desktop, not of the
lock screen (`hostel acquire macos-desktop`, `hostel screenshot`, `hostel
release`).

### 3.6 Mac app QA

Some apps need more than a lease can do, because `hostel` is not an
administrator:

- **Apps in `/Applications`.** Install the apps that the tests use and that must
  stay, for example Google Chrome. A lease can install apps only in
  `~/Applications`, and cleanup of a desktop lease deletes them.
- **Privacy permissions** (Accessibility, Input Monitoring, Screen Recording) for
  the app under test. Start the app once in a lease. Then, as an administrator on
  the Mac, turn on the permission for it in System Settings > Privacy &
  Security. macOS keeps the permission for later builds while the app's bundle
  ID and signing certificate stay the same (section 3.7). An unsigned or ad hoc
  signed build changes its signature with each build and asks again. A
  configuration profile (PPPC) cannot do this without MDM, and no profile can
  turn on Screen Recording.
- **System log.** `log show` and `log stream` need an administrator. If leases
  must read the system log, let `hostel` run only `log`:

  ```sh
  echo 'hostel ALL=(root) NOPASSWD: /usr/bin/log' | sudo tee /etc/sudoers.d/hostel-log
  sudo chmod 440 /etc/sudoers.d/hostel-log && sudo visudo -c
  ```

  The system log can contain data of other processes. Without this, an app's
  own `os_log` lines are on stderr when the app runs with `OS_ACTIVITY_DT_MODE=YES`
  (`tools/macos-desktop/GUIDE.md`).

### 3.7 Signing (optional)

For signed builds (for example Mac Catalyst apps for Mac QA). The administrator
who sets up the Mac does this. Every lease on this Mac can then sign with the
certificate, also leases of other users. Use a certificate and an API key only
for Hostel, so that you can revoke them alone.

1. **The device.** Register the Mac's provisioning UDID in the Apple team
   (System Information > Hardware > Provisioning UDID).
2. **The certificate.** Make an "Apple Development" certificate only for Hostel,
   and export it with its private key as a `.p12`.
3. **The keychain.** As `hostel` (for example `sudo -iu hostel`), make a separate
   keychain, import the certificate without export rights (`-x`), and add the
   keychain to the search list:

   ```sh
   d=~/.config/hostel-signing; mkdir -p -m 700 $d
   (umask 077; openssl rand -hex 16 > $d/password)
   kc=~/Library/Keychains/hostel-signing.keychain-db
   security create-keychain -p "$(cat $d/password)" $kc
   security set-keychain-settings $kc
   security unlock-keychain -p "$(cat $d/password)" $kc
   security import AppleWWDRCAG3.cer -k $kc          # Apple's intermediate certificate
   security import hostel-signing.p12 -k $kc -x -T /usr/bin/codesign -T /usr/bin/security
   security set-key-partition-list -S apple-tool:,apple: -s -k "$(cat $d/password)" $kc
   security list-keychains -d user -s ~/Library/Keychains/login.keychain-db $kc
   ```

   Download `AppleWWDRCAG3.cer` from apple.com/certificateauthority. Without it,
   `security find-identity` finds "0 valid identities". Delete the `.p12` and
   its password from the Mac after the import.
4. **The App Store Connect API key** (role Developer), so that `xcodebuild
   -allowProvisioningUpdates` makes the profiles without an Apple account:

   ```sh
   (umask 077; cp AuthKey_<KEY_ID>.p8 ~/.config/hostel-signing/)
   ```

   Then add to the machine's `env` (`hostel config edit`):
   `HOSTEL_SIGNING_KEYCHAIN = "/Users/hostel/Library/Keychains/hostel-signing.keychain-db"`,
   `ASC_KEY_ID`, `ASC_ISSUER_ID`, and
   `ASC_KEY_PATH = "/Users/hostel/.config/hostel-signing/AuthKey_<KEY_ID>.p8"`.

Over SSH a keychain is locked. A lease unlocks it before a build:

```sh
security unlock-keychain -p "$(cat ~/.config/hostel-signing/password)" "$HOSTEL_SIGNING_KEYCHAIN"
xcodebuild ... -allowProvisioningUpdates -authenticationKeyPath "$ASC_KEY_PATH" \
  -authenticationKeyID "$ASC_KEY_ID" -authenticationKeyIssuerID "$ASC_ISSUER_ID"
```

Check: in a lease, `security find-identity -v -p codesigning` lists the
certificate after the unlock. Tested on `home/macbook` (2026-11-12) with a
self-signed test certificate: the unlock and `codesign` worked over SSH, and the
private key could not be exported.

When the Mac leaves Hostel, revoke the certificate and the API key.

## 4. Windows

Windows 11, as an administrator, in PowerShell.

### 4.1 OpenSSH Server

```powershell
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
Set-Service sshd -StartupType Automatic
Start-Service sshd
```

### 4.2 Join

Run the printed command in an administrator PowerShell. It creates `hostel`,
installs the server key, and installs the scheduled task "Hostel machine
connect" as `LOCAL SERVICE` (log: `C:\ProgramData\hostel\machine-connect.log`).
The token is in `C:\ProgramData\hostel`, which `hostel` cannot read.

Check: the log says that the machine is connected, and `hostel status` shows it.

### 4.3 Build tools

Install what the builds need for all users: the .NET SDK (or use the mise tool
`dotnet:<version>` in the machine's `tools`), Visual Studio Build Tools or the
Windows SDK (`signtool`), and PowerShell 7 (jobs use it when it is in `PATH`).
Microsoft Defender can slow builds; if it blocks the helper, add an exclusion
for `C:\Users\hostel\hostel`.

### 4.4 Desktop (`windows-desktop`)

SSH sessions cannot show windows. A scheduled task of the desktop user starts
`hostel helper desktop serve` as `hostel` in the desktop session, and lease
commands (`desktop`, `winapp`, screenshots) go to it. The desktop must stay
signed in, connected, and unlocked.

Run this as an administrator while the desktop user is signed in:

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
# Keeps "hostel helper desktop serve" running as hostel on this desktop. It
# ends when the helper changes; then this starts the new one.
$dir = 'C:\ProgramData\hostel\desktop'
while ($true) {
  try {
    $pw = ConvertTo-SecureString (Get-Content "$dir\cred" -Raw).Trim() -AsPlainText -Force
    $cred = New-Object Management.Automation.PSCredential("$env:COMPUTERNAME\hostel", $pw)
    $p = Start-Process C:\Users\hostel\hostel\bin\hostel.exe -ArgumentList 'helper','desktop','serve' -Credential $cred -LoadUserProfile -WorkingDirectory C:\Users\hostel -WindowStyle Hidden -PassThru
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

Then add `windows-desktop` to the machine's `tools`. A lease can send keys and
clicks to every window on this desktop that is not elevated, also to the desktop
user's windows: use a desktop user that has nothing else open.

Check: a lease with `windows-desktop` gives a screenshot (`hostel screenshot`).

### 4.5 Signing (optional)

For signed packages (MSIX). The package certificate goes in the computer's store
and is trusted there; `hostel` needs read access to its private key. Every lease
on this computer can then sign with it. Use a development certificate only for
Hostel.

```powershell
$c = Import-PfxCertificate -FilePath dev.pfx -CertStoreLocation Cert:\LocalMachine\My -Password (Read-Host -AsSecureString)
Import-Certificate -FilePath dev.cer -CertStoreLocation Cert:\LocalMachine\TrustedPeople | Out-Null
$k = [Security.Cryptography.X509Certificates.RSACertificateExtensions]::GetRSAPrivateKey($c)
icacls (Join-Path "$env:ProgramData\Microsoft\Crypto\Keys" $k.Key.UniqueName) /grant 'hostel:R'
```

A lease signs with `signtool sign /fd SHA256 /sm /sha1 <thumbprint> <file>`.
Without the `icacls` step, signtool says "No certificates were found that met
all the given criteria" (found on `home/win`, 2026-11-12).

## 5. Linux

1. Run the printed command with `sudo`. It installs the systemd unit
   `hostel-machine-connect.service` (user `hostel-connect`; log: `journalctl -u
   hostel-machine-connect`).
2. The SSH server runs on port 22 and allows key login for `hostel`, with TCP
   forwarding on (the OpenSSH default; tunnels need it).
3. For `android-emulator`: KVM. `hostel` must read and write `/dev/kvm`
   (usually the group `kvm`: `sudo usermod -aG kvm hostel`). A virtual machine
   needs nested virtualization.

## 6. Check the machine

On your own computer:

```sh
hostel status <name>        # free, with its tools
hostel tools                # the machine is listed for each of its tools
```

Then one short lease for each screen tool, with a screenshot:

```sh
hostel acquire <tool> --task "check <name>"
hostel screenshot -o check.png
hostel release
```

## 7. On-demand machines

A provider such as Namespace creates machines from templates when leases need
them, and destroys them later. There is no machine to set up by hand:

- Log in the provider's CLI on the server (`hostel config providers login <provider>`, or
  the app: Rooms, the provider).
- Add a template with its tools and limits (`docs/namespace.md`, and the skill
  `hostel-machines`).
- The image must have what the tools need (Xcode for `xcode`; a desktop for
  `macos-desktop`). Signing material for on-demand machines needs a secret
  store, which Hostel does not have yet.

## 8. By hand, without `machine join`

Use this only when `machine join` cannot run (for example a machine that the
server reaches over SSH, `deploy-vps.md`, section 6, "By hand"). These are the
steps that `join` does for the user and the key.

**macOS:**

```sh
sudo sysadminctl -addUser hostel -fullName "Hostel" -password -     # a strong password; Hostel does not use it
sudo -u hostel mkdir -p -m 700 /Users/hostel/.ssh
echo '<the output of hostel admin pubkey>' | sudo -u hostel tee -a /Users/hostel/.ssh/authorized_keys
sudo -u hostel chmod 600 /Users/hostel/.ssh/authorized_keys
```

**Windows** (a local user without administrator rights, with a random password
that nobody keeps):

```powershell
$b = New-Object byte[] 32; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
$pw = ConvertTo-SecureString ([Convert]::ToBase64String($b) + 'aA1!') -AsPlainText -Force
New-LocalUser -Name hostel -Password $pw -PasswordNeverExpires -AccountNeverExpires
Add-LocalGroupMember -Group Users -Member hostel
# The profile must exist before the first SSH login.
$cred = New-Object Management.Automation.PSCredential('hostel', $pw)
Start-Process cmd.exe -ArgumentList '/c','exit' -Credential $cred -LoadUserProfile -WorkingDirectory C:\Windows -Wait
# The server key; the SSH server accepts the file only with these permissions.
New-Item -ItemType Directory -Force C:\Users\hostel\.ssh | Out-Null
Set-Content C:\Users\hostel\.ssh\authorized_keys '<the output of hostel admin pubkey>'
icacls C:\Users\hostel\.ssh\authorized_keys /inheritance:r /grant 'SYSTEM:F' /grant 'hostel:F'
```

Cleanup restores `authorized_keys` from `~/hostel/authorized_keys.base` after the
first lease; a new server key goes into both files.
