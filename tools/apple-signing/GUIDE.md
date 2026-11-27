Signing for Apple apps: the team's "Apple Development" certificate in a
keychain of this lease, and the team's App Store Connect API key, so that
Xcode makes the development profiles. Use it for apps that must be signed to
run with their entitlements (push, keychain groups, app groups, extensions),
for example a Mac Catalyst app on the macOS desktop:

  hostel acquire xcode macos-desktop apple-signing --task "QA the Mac app"
  hostel run -- 'xcodebuild -scheme MyApp -destination "platform=macOS,variant=Mac Catalyst" \
    -derivedDataPath build -allowProvisioningUpdates \
    -authenticationKeyPath "$ASC_KEY_PATH" -authenticationKeyID "$ASC_KEY_ID" \
    -authenticationKeyIssuerID "$ASC_ISSUER_ID" build'

Use the project's automatic signing ("Apple Development"). The profiles that
Xcode makes stay on the machine for later leases; the certificate and the key
are only in this lease. HOSTEL_SIGNING_KEYCHAIN is the keychain, and
HOSTEL_SIGNING_TEAM the team ID of the certificate.

Do not:
- pass -allowProvisioningDeviceRegistration, or register devices in another
  way: an admin registers each Mac by hand;
- sign ad hoc (CODE_SIGNING_ALLOWED=NO, CODE_SIGN_IDENTITY=-): the app then
  loses its restricted entitlements and fails at run time, often without a
  clear error (errSecMissingEntitlement, -34018);
- copy the certificate, the key, or their files out of the lease.

If the build says that the device (this Mac) is not in the profile, the Mac is
not registered in the Apple team: stop and tell the owner. Its Provisioning
UDID: system_profiler SPHardwareDataType | grep "Provisioning UDID".
