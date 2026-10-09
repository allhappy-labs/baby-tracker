# Siri commands verification

Date: 2026-09-27. Branch: `feat/siri-commands`.

## Completed checks

- `make test`: `OK: 842 tests`, including repeated sleep start and read-only sleep stop coverage. The repository's literal count gate passed.
- `xcodebuild -quiet -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -derivedDataPath /path/to/artifact CODE_SIGNING_ALLOWED=NO build`: raw Xcode exit 0. The RTK-filtered clean-build output contained a misleading `error ... exit code 0` line, so the raw Xcode result was checked separately.
- Inspected the clean-built `BabyTracker.app/Metadata.appintents/extract.actionsdata`: six Siri action types and nine registered App Shortcuts, with the nine approved phrase templates. `root.ssu.yaml` includes the spoken application name `Baby Tracker`.
- Inspected the built `BabyTracker.app/Info.plist`: `CFBundleDisplayName` is `Baby Tracker`.

## Device acceptance still needed

The build and metadata show what the app offers the system. They do not prove that Siri recognizes each utterance, binds the wet/dirty/mixed and left/right preset values correctly, speaks the intended response, runs with the phone locked, or updates a real device's History and Live Activity. Use a test baby and check all nine phrases on an iPhone after installing this build. Test start/stop and already-running cases without altering real tracking data.

The first sandboxed build could not access CoreSimulator and Swift caches. The same build completed after rerunning with local Xcode service access; this was an environment restriction, not a source failure.
