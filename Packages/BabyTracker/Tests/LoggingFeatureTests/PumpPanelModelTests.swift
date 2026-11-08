import Testing
import Foundation
import BabyCore
@testable import BabyStore
@testable import LoggingFeature

@MainActor @Suite struct PumpPanelModelTests {
    @Test func startStartsAPumpAndThenOffersOnlyEditRunning() async throws {
        let (store, _, _) = try fixture()
        let model = PumpPanelModel(store: store, mode: .start)
        #expect(model.runningPumpID == nil)
        #expect(model.canSave)
        #expect(!model.showsAmount)
        model.side = .left
        let pump = try await model.startNow()
        #expect(pump.side == .left)
        #expect(pump.isRunning)
        let again = PumpPanelModel(store: store, mode: .start)
        #expect(again.runningPumpID == pump.id)
        #expect(!again.canSave)
        await #expect(throws: PumpError.alreadyRunning) { _ = try await again.startNow() }
    }

    /// A pump the other phone started while this panel is open: Start turns
    /// into Edit running at the next store change.
    @Test func aPumpStartedElsewhereTurnsStartIntoEditRunning() throws {
        let (store, _, _) = try fixture()
        let model = PumpPanelModel(store: store, mode: .start)
        let other = try store.startPump(side: .right)
        model.reload()
        #expect(model.runningPumpID == other.id)
        #expect(!model.canSave)
    }

    @Test func aPastSessionSavesWithItsAmountAndNote() throws {
        let (store, _, clock) = try fixture()
        let model = PumpPanelModel(store: store, mode: .past)
        #expect(model.end != nil)
        #expect(model.showsAmount)
        model.side = .right
        model.start = clock.now.addingTimeInterval(-1800)
        model.end = clock.now.addingTimeInterval(-600)
        model.volumeML = 110
        model.note = " evening "
        #expect(model.validationMessage == nil)
        #expect(model.canSave)
        try model.save()
        let saved = try #require(try store.lastFinishedPump())
        #expect(saved.side == .right)
        #expect(saved.volumeML == 110)
        #expect(saved.note == "evening")
    }

    @Test func aSessionLongerThanSixHoursOrInTheFutureIsRefused() throws {
        let (store, _, clock) = try fixture()
        let model = PumpPanelModel(store: store, mode: .past)
        model.start = clock.now.addingTimeInterval(-7 * 3600)
        model.end = clock.now
        #expect(model.validationMessage == PumpPanelCopy.tooLong)
        #expect(!model.canSave)
        #expect(throws: EditModeError.invalid) { try model.save() }
        model.end = clock.now.addingTimeInterval(60)
        #expect(model.validationMessage == TimeSpanCopy.inTheFuture)
        model.start = clock.now.addingTimeInterval(-600)
        model.end = clock.now.addingTimeInterval(-900)
        #expect(model.validationMessage == TimeSpanCopy.endBeforeStart)
        #expect(try store.pumps(from: .distantPast, to: .distantFuture).isEmpty)
    }

    /// The strip's tap: side, Start and note editable, End reads Running, no
    /// amount yet, and saving keeps it running.
    @Test func editingARunningPumpKeepsItRunning() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.startPump(side: .both)
        let model = PumpPanelModel(store: store, mode: .edit(pump.id))
        #expect(model.isRunning)
        #expect(model.isEditing)
        #expect(model.end == nil)
        #expect(!model.showsAmount)
        model.side = .left
        model.start = pump.startedAt.addingTimeInterval(-120)
        model.note = "night"
        try model.save()
        #expect(pump.isRunning)
        #expect(pump.side == .left)
        #expect(pump.note == "night")
        #expect(pump.startedAt == clock.now.addingTimeInterval(-120))
    }

    @Test func theOtherPhonesStopShowsUpWhileTheRunningPanelIsOpen() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.startPump(side: .both)
        let model = PumpPanelModel(store: store, mode: .edit(pump.id))
        clock.advance(by: 900)
        try store.stopPump(pump)
        model.reload()
        #expect(!model.isRunning)
        #expect(model.end == pump.endedAt)
        #expect(model.showsAmount)
        // Saving now writes the finished session, never a reopened one.
        model.volumeML = 70
        try model.save()
        #expect(pump.endedAt != nil)
        #expect(pump.volumeML == 70)
    }

    /// The other parent stops the session and then enters 90 ml while this
    /// panel is open on the running session. Saving here (say, after fixing
    /// the side) must keep their amount, not send "no amount".
    @Test func theOtherPhonesAmountIsKeptWhenTheRunningPanelSaves() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.startPump(side: .both)
        let model = PumpPanelModel(store: store, mode: .edit(pump.id))
        clock.advance(by: 900)
        try store.stopPump(pump)
        model.reload()
        try store.setPumpAmount(pump, volumeML: 90)
        model.reload()
        #expect(model.hasAmount)
        #expect(model.volumeML == 90)
        model.side = .left
        try model.save()
        #expect(pump.volumeML == 90)
        #expect(pump.side == .left)
    }

    /// An amount this parent has typed is theirs: a reload does not replace it.
    @Test func anAmountTypedHereSurvivesAReload() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-900), to: clock.now, volumeML: 50, note: nil)
        let model = PumpPanelModel(store: store, mode: .edit(pump.id))
        model.volumeML = 70
        try store.setPumpAmount(pump, volumeML: 60)
        model.reload()
        #expect(model.volumeML == 70)
    }

    @Test func deleteSoftDeletesAndAGoneSessionCannotSave() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-900), to: clock.now, volumeML: nil, note: nil)
        let model = PumpPanelModel(store: store, mode: .edit(pump.id))
        try model.delete()
        model.reload()
        #expect(model.isGone)
        #expect(!model.canSave)
        #expect(try store.pump(id: pump.id) == nil)
        #expect(throws: EditModeError.gone) { try model.save() }
        #expect(PumpPanelModel(store: store, mode: .edit(UUID())).isGone)
    }

    @Test func howMuchSavesAnAmountAndSkipLeavesItEmpty() throws {
        let (store, _, clock) = try fixture()
        let earlier = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-9000), to: clock.now.addingTimeInterval(-8000), volumeML: 70, note: nil)
        let pump = try store.startPump(side: .both)
        clock.advance(by: 600)
        try store.stopPump(pump)
        let skipped = PumpAmountModel(store: store, pumpID: pump.id)
        #expect(skipped.volumeML == earlier.volumeML)   // seeded from the last amount
        #expect(skipped.endedAt == pump.endedAt)
        #expect(pump.volumeML == nil)                    // Skip writes nothing
        let model = PumpAmountModel(store: store, pumpID: pump.id, unit: .ounces)
        model.volumeML = Volume(ounces: 3).millilitres
        #expect(model.canSave)
        try model.save()
        #expect(pump.volumeML == 88.72)
        try store.softDelete(pump)
        #expect(throws: EditModeError.gone) { try model.save() }
    }

    /// Start mode's Log, as the bottle sheet's: a finished session of the
    /// amount at the chosen time, off at 0 and while a pump runs.
    @Test func logWritesAFinishedSessionOfTheAmount() throws {
        let (store, _, clock) = try fixture()
        let model = PumpPanelModel(store: store, mode: .start)
        #expect(model.volumeML == 0)
        #expect(!model.canLog)
        model.volumeML = model.chips[2]
        model.side = .right
        model.when = clock.now.addingTimeInterval(-600)
        #expect(model.canLog)
        try model.logNow()
        let saved = try #require(try store.lastFinishedPump())
        #expect(saved.volumeML == 90)
        #expect(saved.side == .right)
        #expect(saved.startedAt == clock.now.addingTimeInterval(-600))
        #expect(saved.endedAt == saved.startedAt)
        // The next sheet starts from that amount.
        #expect(PumpPanelModel(store: store, mode: .start).volumeML == 90)
        _ = try store.startPump(side: .both)
        model.reload()
        #expect(!model.canLog)
        #expect(throws: EditModeError.invalid) { try model.logNow() }
    }

    /// The bottle's chips are formula amounts; the pump has its own, and
    /// the bottle's step, kept inside 0–1000 ml.
    @Test func theAmountControlHasPumpChipsAndTheBottlesStep() throws {
        let (store, _, _) = try fixture()
        let model = PumpPanelModel(store: store, mode: .past)
        #expect(model.chips == [30, 60, 90, 120])
        #expect(PumpPanelModel(store: store, mode: .past, unit: .ounces).chips.map { Volume(millilitres: $0).ounces.rounded() } == [1, 2, 3, 4])
        model.step(-1)
        #expect(model.volumeML == 0)
        model.step(1)
        #expect(model.volumeML == 5)
        model.volumeML = PumpRules.maxVolumeML
        model.step(1)
        #expect(model.volumeML == PumpRules.maxVolumeML)
    }

    /// Dialling an amount down to 0 in the edit panel clears it.
    @Test func zeroInTheEditPanelClearsTheAmount() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.logPump(side: .left, from: clock.now.addingTimeInterval(-900), to: clock.now, volumeML: 50, note: nil)
        let model = PumpPanelModel(store: store, mode: .edit(pump.id))
        #expect(model.hasAmount)
        model.volumeML = 0
        #expect(model.canSave)
        try model.save()
        #expect(pump.volumeML == nil)
    }

    /// How much? corrects the side, as a stopped bottle's sheet corrects its
    /// contents, and keeps the session's times and note.
    @Test func howMuchCorrectsTheSide() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.startPump(side: .both)
        clock.advance(by: 600)
        let end = try store.stopPump(pump)
        let model = PumpAmountModel(store: store, pumpID: pump.id)
        #expect(model.side == .both)
        model.side = .left
        model.volumeML = 60
        try model.save()
        #expect(pump.side == .left)
        #expect(pump.volumeML == 60)
        #expect(pump.endedAt == end)
    }

    /// Saving with nothing entered is the same as Skip: no amount, not 0 ml.
    @Test func savingZeroLeavesTheAmountEmpty() throws {
        let (store, _, clock) = try fixture()
        let pump = try store.startPump(side: .left)
        clock.advance(by: 600)
        try store.stopPump(pump)
        let model = PumpAmountModel(store: store, pumpID: pump.id)
        #expect(model.volumeML == 0)
        try model.save()
        #expect(pump.volumeML == nil)
    }
}
