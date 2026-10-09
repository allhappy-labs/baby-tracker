import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import HomeFeature

// The editor opened by tapping the running dial. Its rules are the store's
// (`RunningTimerEdit`), so these tests are about what it offers, what it says
// when an edit would be refused, and that it sends only what changed.

@MainActor
private func freshStore() throws -> (BabyStore, UUID, TestDateProvider) {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    return (store, baby.id, clock)
}

/// Left 10 min, a 50 ml breast-milk bottle, then the right side running for
/// a minute.
///
/// Two finished parts on one still-running timer can no longer be built by a
/// live switch — one-part timers give each part its own timer. Built
/// directly here, the way a legacy remote timer (`RemoteTimerState`) would
/// leave it.
@MainActor
private func feedWithParts() throws -> (BabyStore, UUID, TestDateProvider, ActiveTimer) {
    let (store, babyID, clock) = try freshStore()
    let timer = try store.startPart(.breast(.left), for: babyID)
    let start = timer.startedAt
    clock.advance(by: 600)
    let bottleEnd = clock.now
    timer.completedComponents = [
        .breast(id: UUID(), side: .left, seconds: 600, startedAt: start, endedAt: bottleEnd),
        .bottle(id: UUID(), volumeML: 50, contents: .breastMilk),
    ]
    timer.currentSide = .right
    timer.currentSideStartedAt = bottleEnd
    try store.context.save()
    clock.advance(by: 60)
    return (store, babyID, clock, timer)
}

@MainActor
@Test func theEditorIsOnlyOfferedWhileATimerRuns() throws {
    let (store, babyID, _) = try freshStore()
    let home = HomeViewModel(store: store, babyID: babyID)
    #expect(home.makeTimerEditor() == nil)

    let timer = try store.startPart(.breast(.left), for: babyID)
    let editor = try #require(home.makeTimerEditor())
    #expect(editor.id == timer.id)
    #expect(editor.startedAt == timer.startedAt)
}

@MainActor
@Test func theEditorShowsTheFinishedPartsAndTheRunningSideReadOnly() throws {
    let (store, babyID, _, timer) = try feedWithParts()
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    #expect(editor.parts.map(\.title) == ["Left breast", "Bottle"])
    #expect(editor.parts.map(\.id) == timer.completedComponents.map(\.id))
    #expect(editor.parts[0].seconds == 600)
    #expect(editor.parts[1].volumeML == 50)
    #expect(editor.parts[1].contents == .breastMilk)
    #expect(editor.runningSideText?.hasPrefix("Right breast · since ") == true)
    #expect(editor.title == "Edit feed")
    #expect(editor.durationText(for: editor.parts[0]) == "10m")
    #expect(editor.volumeText(for: editor.parts[1]) == "50 ml")
}

@MainActor
@Test func aSleepEditsItsStartOnly() throws {
    let (store, babyID, _) = try freshStore()
    _ = try store.startSleep(for: babyID)
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    #expect(editor.title == "Edit sleep")
    #expect(editor.parts.isEmpty)
    #expect(editor.runningSideText == nil)
}

@MainActor
@Test func aRunningBottleOffersItsStartForEditing() throws {
    let (store, babyID, clock) = try freshStore()
    let timer = try store.startPart(.bottle(.formula), for: babyID)
    clock.advance(by: 60)
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())
    editor.shiftStart(by: -300)
    #expect(editor.canSave)
    #expect(editor.startedAt == timer.startedAt.addingTimeInterval(-300))
}

@MainActor
@Test func aBottleAddedLaterBoundsTheStartPickerAtItsOwnStart() throws {
    let (store, babyID, clock) = try freshStore()
    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 120)
    // A bottle beginning after a breast part on the same still-running timer
    // can no longer be built live — built directly, as a legacy remote timer
    // would leave it.
    let bottleStart = clock.now
    timer.completedComponents = [.breast(id: UUID(), side: .left, seconds: 120, startedAt: timer.startedAt, endedAt: bottleStart)]
    timer.currentSide = nil
    timer.currentBottleStartedAt = bottleStart
    clock.advance(by: 60)

    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())
    #expect(editor.startRange.upperBound == bottleStart)
    editor.startedAt = bottleStart.addingTimeInterval(1)
    #expect(!editor.canSave)
    #expect(editor.validationMessage?.contains("bottle") == true)
}

@MainActor
@Test func editingTheSecondaryTimerChangesOnlyItsOwnStart() async throws {
    let (store, babyID, clock) = try freshStore()
    let feed = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 60)
    let sleep = try store.startSleep(for: babyID)
    let sleepStart = sleep.startedAt
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor(timerID: feed.id))
    #expect(editor.id == feed.id)
    editor.shiftStart(by: -300)
    #expect(try await editor.save())
    #expect(feed.startedAt == sleepStart.addingTimeInterval(-360))
    #expect(sleep.startedAt == sleepStart)
    #expect(HomeViewModel(store: store, babyID: babyID).makeTimerEditor(timerID: UUID()) == nil)
}

@MainActor
@Test func nothingChangedCannotBeSavedAndSaysNothing() throws {
    let (store, babyID, _, _) = try feedWithParts()
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    #expect(!editor.canSave)
    #expect(editor.validationMessage == nil)
}

@MainActor
@Test func savingAMovedStartWritesItAndMovesTheTiedSide() async throws {
    let (store, babyID, clock) = try freshStore()
    let timer = try store.startPart(.breast(.left), for: babyID)
    let original = timer.startedAt
    clock.advance(by: 300)
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    editor.shiftStart(by: -600)
    // The side began with the feed, so the editor shows it moving too.
    #expect(editor.runningSideText == "Left breast · since \(HomeCopy.clockTime(original.addingTimeInterval(-600), zone: timer.timeZoneID))")
    #expect(editor.canSave)
    #expect(try await editor.save())

    #expect(timer.startedAt == original.addingTimeInterval(-600))
    #expect(timer.currentSideStartedAt == original.addingTimeInterval(-600))
    #expect(timer.elapsedSeconds(now: clock.now) == 900)
}

@MainActor
@Test func savingEditedPartsKeepsTheirIDsAndLeavesTheStartAlone() async throws {
    let (store, babyID, _, timer) = try feedWithParts()
    let start = timer.startedAt
    let left = timer.completedComponents[0].id
    let bottle = timer.completedComponents[1].id
    let leftEnd = try #require(timer.completedComponents[0].endedAt)
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    editor.parts[0].seconds = 300
    editor.parts[1].volumeML = 70
    editor.parts[1].contents = .formula
    #expect(try await editor.save())

    #expect(timer.startedAt == start)
    #expect(timer.completedComponents == [
        .breast(id: left, side: .left, seconds: 300,
                startedAt: leftEnd.addingTimeInterval(-300), endedAt: leftEnd),
        .bottle(id: bottle, volumeML: 70, contents: .formula),
    ])
    #expect(timer.currentSide == .right)
}

@MainActor
@Test func aStartInTheFutureOrOutOfRangeIsExplainedAndNotSaved() throws {
    let (store, babyID, clock) = try freshStore()
    let timer = try store.startSleep(for: babyID)
    clock.advance(by: 600)
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    editor.startedAt = clock.now.addingTimeInterval(120)
    #expect(!editor.canSave)
    #expect(editor.validationMessage == "The start can't be in the future.")

    // The store refuses a start more than a week back with the same error;
    // a start in the past must not be called "in the future".
    editor.startedAt = clock.now.addingTimeInterval(-8 * 86_400)
    #expect(!editor.canSave)
    #expect(editor.validationMessage == "That start time is outside the allowed range.")
    #expect(timer.startedAt != editor.startedAt)
}

@MainActor
@Test func aStartAfterTheRunningSideBeganIsExplainedAndOutsideThePicker() throws {
    let (store, babyID, clock, timer) = try feedWithParts()
    let sideStart = timer.currentSideStartedAt
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    // Untied: the right side began after the feed did, so the picker stops there.
    #expect(editor.startRange.upperBound == sideStart)
    #expect(editor.startRange.lowerBound == clock.now.addingTimeInterval(-86_400))

    // An untied side stays where it began whatever the start does.
    let sideText = editor.runningSideText
    editor.shiftStart(by: -300)
    #expect(editor.runningSideText == sideText)

    editor.startedAt = sideStart.addingTimeInterval(30)
    #expect(!editor.canSave)
    #expect(editor.validationMessage?.hasPrefix("The start can't be after the right breast began") == true)
}

@MainActor
@Test func partsLongerThanTheFeedHasRunAreExplained() throws {
    let (store, babyID, _, _) = try feedWithParts()
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    editor.parts[0].seconds = 660
    #expect(!editor.canSave)
    #expect(editor.validationMessage == "The finished parts add up to more time than the feed has run. Shorten a part or move the start earlier.")

    // Moving the start back makes the room.
    editor.shiftStart(by: -300)
    #expect(editor.canSave)
    #expect(editor.validationMessage == nil)
}

@MainActor
@Test func shiftingTheStartStaysInsideThePicker() throws {
    let (store, babyID, clock) = try freshStore()
    _ = try store.startSleep(for: babyID)
    clock.advance(by: 120)
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    editor.shiftStart(by: 300)
    #expect(editor.startedAt == editor.startRange.upperBound)
    editor.shiftStart(by: -3 * 86_400)
    #expect(editor.startedAt == editor.startRange.lowerBound)
}

@MainActor
@Test func aTimerStoppedOnTheOtherPhoneSaysWhereToEditIt() async throws {
    let (store, babyID, clock) = try freshStore()
    let timer = try store.startPart(.breast(.left), for: babyID)
    clock.advance(by: 60)
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())
    editor.shiftStart(by: -300)

    _ = try store.stopFeed(timer)

    #expect(try await !editor.save())
    #expect(!editor.canSave)
    #expect(editor.validationMessage == "This timer was stopped on another phone — edit the feed in History.")

    // Noticed without a save attempt, too, when the store changes under it.
    let sleep = try store.startSleep(for: babyID)
    let sleepEditor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())
    _ = try store.stopSleep(sleep)
    sleepEditor.refreshAvailability()
    #expect(sleepEditor.validationMessage == "This timer was stopped on another phone — edit the sleep in History.")
}

/// A read-only caregiver sees the dial but cannot open the editor: the store
/// would refuse the write, and an editor that cannot save is a trap.
@MainActor
@Test func aReadOnlyCaregiverCannotOpenTheEditor() throws {
    let clock = TestDateProvider(Date(timeIntervalSince1970: 1_700_000_000))
    let store = try BabyStore.inMemory(dateProvider: clock)
    let baby = try store.addBaby(name: "Mira", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
    _ = try store.startPart(.breast(.left), for: baby.id)
    let realm = RealmID.shared(
        canonicalOrigin: "https://sync.example",
        serverInstanceID: UUID(),
        familyID: UUID()
    )
    let checkpoint = UUID()
    try store.beginTransition(to: realm, checkpointID: checkpoint)
    try store.commitTransition(checkpointID: checkpoint)
    let home = HomeViewModel(store: store, babyID: baby.id)
    #expect(home.makeTimerEditor() != nil)

    try store.setWritePermission(.viewer, for: realm)
    #expect(home.makeTimerEditor() == nil)
}

@MainActor
@Test func theEditorsCopyIsSentenceCaseAndLabelled() {
    #expect(RunningTimerEditCopy.dialHint == "Edit the start time and the parts so far")
    #expect(RunningTimerEditCopy.startEarlier == "5 minutes earlier")
    #expect(RunningTimerEditCopy.startLater == "5 minutes later")
    #expect(RunningTimerEditCopy.durationAccessibilityLabel("Left breast") == "Left breast duration")
    #expect(RunningTimerEditCopy.volumeAccessibilityLabel("Bottle 2") == "Bottle 2 volume")
}

/// The other phone made the same correction while the sheet was open: the
/// store has nothing left to change. The user's edit is what the timer says,
/// so the sheet saves and closes rather than dead-ending with Save greyed out.
@MainActor
@Test func theSameCorrectionAlreadyMadeElsewhereCountsAsSaved() async throws {
    let (store, babyID, clock) = try freshStore()
    let timer = try store.startPart(.breast(.left), for: babyID)
    let original = timer.startedAt
    clock.advance(by: 300)
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())
    editor.shiftStart(by: -300)

    try store.editRunningTimer(id: timer.id, startedAt: original.addingTimeInterval(-300), components: [])

    #expect(try await editor.save())
    #expect(editor.validationMessage == nil)
    #expect(timer.startedAt == original.addingTimeInterval(-300))
}

/// Stated, not measured, so it has to follow what is on the sheet.
@MainActor
@Test func theSheetGrowsWithWhatIsOnIt() throws {
    let (store, babyID, clock) = try freshStore()
    let sleep = try store.startSleep(for: babyID)
    let sleepEditor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())
    let startOnly = sleepEditor.sheetHeight
    sleepEditor.startedAt = clock.now.addingTimeInterval(600)
    #expect(sleepEditor.sheetHeight > startOnly)   // the message row
    clock.advance(by: 60)
    _ = try store.stopSleep(sleep)

    let feed = try store.startPart(.breast(.left), for: babyID)
    let running = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor()).sheetHeight
    #expect(running > startOnly)
    clock.advance(by: 600)
    // Two finished parts on one still-running timer can no longer be built
    // live — built directly, as a legacy remote timer would leave it.
    feed.completedComponents = [
        .breast(id: UUID(), side: .left, seconds: 600, startedAt: feed.startedAt, endedAt: clock.now),
        .bottle(id: UUID(), volumeML: 50, contents: .formula),
    ]
    feed.currentSide = .right
    feed.currentSideStartedAt = clock.now
    try store.context.save()
    let withParts = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor()).sheetHeight
    #expect(withParts > running)
}

/// The `-uiTestJoinedFeed` case: Right runs, joined to a Left part that ended
/// three minutes ago. Its start can't move before Left ended — the store
/// refuses that — so the picker stops there, the nudge stops there, and a
/// refusal the editor could not foresee (the other phone lengthened Left) is
/// explained rather than thrown to the generic banner.
@MainActor
@Test func aJoinedPartsStartStopsWhereThePreviousPartEnded() async throws {
    let (store, babyID, clock) = try freshStore()
    store.breakLimits = { .standard }
    let left = try store.logBreast(side: .left, from: clock.now.addingTimeInterval(-722),
                                   to: clock.now.addingTimeInterval(-180), for: babyID)
    let timer = try store.startPart(.breast(.right), for: babyID)
    #expect(timer.sessionID != nil)
    let leftEnded = clock.now.addingTimeInterval(-180)
    let editor = try #require(HomeViewModel(store: store, babyID: babyID).makeTimerEditor())

    #expect(editor.startRange.lowerBound == leftEnded)
    editor.shiftStart(by: -300)
    #expect(editor.startedAt == leftEnded)
    #expect(editor.canSave)

    // Set below the picker (as an older copy of the range would allow).
    editor.startedAt = leftEnded.addingTimeInterval(-60)
    #expect(!editor.canSave)
    #expect(editor.validationMessage?.hasPrefix("The start can't be before the previous part ended") == true)

    // The other phone lengthens Left after the editor opened: the store
    // refuses, and the editor says why instead of throwing.
    editor.startedAt = leftEnded
    left.endedAt = leftEnded.addingTimeInterval(60)
    try store.context.save()
    let saved = try await editor.save()
    #expect(!saved)
    #expect(!editor.canSave)
    #expect(editor.validationMessage?.hasPrefix("The start can't be before the previous part ended") == true)
    #expect(timer.startedAt == clock.now)
}
