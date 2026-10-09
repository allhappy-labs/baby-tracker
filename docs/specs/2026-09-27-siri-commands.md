# Siri commands for Baby Tracker

Status: approved. Implementation is tracked in `TASKS.md`; on-device Siri acceptance remains to be verified.

## Goal

Let a parent log a diaper or control feeding and sleep timers by voice, without opening the app. Commands should also appear as actions in Shortcuts. The first release covers the selected baby only. Siri must report the result of a command, especially when no record was made.

## Proposed commands

Say "Hey Siri" followed by one of these phrases. The app name in the phrases helps Siri identify the intended app. These are proposed phrases to register and test on an iPhone; Apple's speech matching may accept similar wording but exact recognition is not guaranteed.

| Spoken phrase | Action | Success response |
| --- | --- | --- |
| "Log a wet diaper in Baby Tracker" | Save a wet diaper at the current time. | "Logged a wet diaper for [baby]." |
| "Log a dirty diaper in Baby Tracker" | Save a dirty diaper at the current time. | "Logged a dirty diaper for [baby]." |
| "Log a mixed diaper in Baby Tracker" | Save a mixed diaper at the current time. | "Logged a mixed diaper for [baby]." |
| "Start left breast feeding in Baby Tracker" | Start a feed timer on the left breast. | "Started left breast feeding for [baby]." |
| "Start right breast feeding in Baby Tracker" | Start a feed timer on the right breast. | "Started right breast feeding for [baby]." |
| "Switch breast in Baby Tracker" | Switch the running breast feed to the opposite side. | "Switched to [side] breast for [baby]." |
| "Stop feeding in Baby Tracker" | End and save the running feed. | "Saved the feeding for [baby]." |
| "Start sleep in Baby Tracker" | Start a sleep timer. | "Started sleep for [baby]." |
| "Stop sleep in Baby Tracker" | End and save the running sleep. | "Saved the sleep for [baby]." |

The same actions should have clear titles and parameters in Shortcuts. A personal Shortcut can be named for a preferred phrase, including one without the app name. The app-provided phrases above are the primary built-in voice experience.

## Behavior rules

1. **Baby selection:** use the app's current selected baby. Include the baby's name in the response. If no baby is available, report that nothing was changed; never silently succeed or choose a different baby.
2. **Diaper logging:** write one entry for each successful invocation, using the app's existing `logDiaper` store operation. Do not add a confirmation step to routine logging. If the write fails, report failure rather than claiming that an entry was logged. Repeating the command deliberately creates another entry, so test Siri's behavior with repeated speech.
3. **Starting a feed:** use `startAnnouncedFeed` so the timer and Live Activity follow the same path as the app. If a feed is already running, leave it unchanged and say that it is already running, naming the current side or component when available. Asking to start the other side must not silently switch sides.
4. **Switching:** require a running breast feed. Use the existing side-switch operation and update the Live Activity. If no feed is running, or the active component is a bottle or solids, report that no side was switched.
5. **Stopping:** require a running feed. Use the existing stop path and end its Live Activity. If no feed is running, report that nothing was saved. If the store rejects an invalid or zero-duration feed, report the failure without claiming success.
6. **Sleep:** use separate start and stop voice commands, so repeating "start" cannot accidentally stop a running sleep. Starting an already running sleep leaves its original start time unchanged and reports that it is already running. Stopping when none is running reports that nothing was saved. Use `startSleep`, `stopSleep`, and the Live Activity start/end paths. The saved nap/night classification follows the app's existing start-time rule; the voice command does not choose a sleep type.
7. **Access and sync:** respect the store's read-only caregiver gate. Persist locally through the normal store path and rely on the existing sync pipeline. A temporary network outage should not make a successful local save sound like a failure.
8. **No hidden fallback:** do not turn an unrecognized feeding request into a default left-breast feed. A generic "log a feed" action may continue opening the app's feed menu.

## Implementation after approval

- Add parameterized App Intents for diaper type and breast side. Expose the existing switch and feed-stop actions as Shortcuts, adapting their spoken results while preserving widget and Live Activity button behavior. Add distinct sleep-start and sleep-stop voice actions while preserving the existing widget and Live Activity sleep toggle. Reuse `WidgetStore` and the existing `BabyStore` methods.
- Register the actions and proposed spoken phrases with an `AppShortcutsProvider`. Expose useful Shortcuts titles and spoken result dialogs, including actionable failure messages.
- Add focused tests for the action routing and edge cases: selected/missing baby, each diaper type, feed or sleep already running, switch without a breast component, stop without a timer, read-only access, and failed writes.
- Build the app and verify generated App Intents metadata. On an iPhone, check that each phrase invokes the intended action with the phone locked and unlocked, that Siri speaks the true outcome, and that History, the timer, and the Live Activity reflect it. Record any phrase that Siri consistently misrecognizes and adjust the published phrases.

## Scope decisions for review

- This first set covers diaper logging, breast-feeding control, and starting/stopping sleep. Bottle amounts, solids, historical timestamps, and selecting a baby by spoken name are later additions.
- A successful diaper command makes a record immediately. A successful start, switch, or stop command changes the active timer immediately. Siri's spoken response is feedback, not an approval prompt.
- Exact speech recognition and locked-phone execution are device acceptance criteria, not claims established by this document.

## References

- Existing intents: `Apps/SharedIntents/LogIntents.swift`
- Shared intent store: `Apps/SharedIntents/WidgetStore.swift`
- Store operations: `Packages/BabyTracker/Sources/BabyStore/BabyStore+Diaper.swift`, `BabyStore+Feeding.swift`
- Apple: [App Shortcuts](https://developer.apple.com/documentation/appintents/app-shortcuts), [App Intents](https://developer.apple.com/documentation/appintents/appintent), [runtime behavior](https://developer.apple.com/documentation/appintents/configuring-the-runtime-behavior-of-your-app-intents)
