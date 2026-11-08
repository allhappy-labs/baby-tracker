import SwiftUI
import Observation
import BabyCore
import BabyStore
import BabyUI

/// Every user-visible string in `SolidsSheet`, kept out of the view so copy is
/// assertable without a simulator and so no string is hard-coded in a body.
public enum SolidsSheetCopy {
    public static let title = LoggingL10n.text("Solids")
    public static let foodsLabel = LoggingL10n.text("Foods")
    public static let foodsPrompt = LoggingL10n.text("Pear, oat")
    /// Says what the commas do, because "Pear, oat" alone does not.
    public static let foodsFooter = LoggingL10n.text("Separate foods with commas")
    /// Shown in place of `foodsFooter` while Save is off, so the disabled
    /// button has a stated reason rather than being inexplicably dead.
    public static let foodsRequiredFooter = LoggingL10n.text("Name at least one food to save")
    public static let amountLabel = LoggingL10n.text("Amount")
    public static let dateTimeLabel = LoggingL10n.text("Date and time")

    /// Free text in, a clean list out: trimmed, with empties dropped so a
    /// trailing comma or a double comma does not become a blank food.
    public static func foods(from text: String) -> [String] {
        text.split(separator: ",")
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
    }

    /// Save is off until there is at least one food.
    ///
    /// An entry with `foods == []` is not merely cosmetic: a solids feed IS
    /// its foods and amount, so an empty one says nothing in History or
    /// anywhere else. The same rule holds in edit mode, so a correction
    /// cannot make one either. The amount picker always has a value, so
    /// foods is the only thing to check.
    public static func canSave(foodsText: String) -> Bool {
        !foods(from: foodsText).isEmpty
    }
}

/// Edit mode's state and writes for a solids entry: its foods, amount, time
/// and note as a draft, saved in one update or deleted.
///
/// Holds the feed's id, not the feed. The other phone may change or delete
/// it while the sheet is open, so every write fetches it afresh, and
/// `reload()` — which the sheet calls on every `store.revision` — notices
/// when it has gone.
@MainActor @Observable
public final class SolidsEditModel {
    private let store: BabyStore
    private let babyID: UUID
    public let feedID: UUID
    /// The solids component being edited. Its id survives every save, so
    /// anything that names the component keeps naming it.
    public let componentID: UUID?

    public var foodsText: String
    public var amount: SolidsAmount
    public var when: Date
    public var note: String
    /// True once the entry has vanished, for example after the other phone
    /// deleted it. The sheet closes on it.
    public private(set) var isGone: Bool

    /// The entry's fields as last read, so `reload()` takes the other phone's
    /// change only into fields the user has not touched.
    private var loaded: Draft

    private struct Draft: Equatable {
        var foodsText: String
        var amount: SolidsAmount
        var when: Date
        var note: String
    }

    public init(store: BabyStore, babyID: UUID, feedID: UUID) {
        self.store = store
        self.babyID = babyID
        self.feedID = feedID
        let feed = Self.fetch(feedID, babyID: babyID, in: store)
        let component = feed?.components.first { $0.solidFoods != nil }
        componentID = component?.id
        let draft = feed.flatMap { feed in component.map { Self.draft(feed, $0) } }
            ?? Draft(foodsText: "", amount: .some, when: store.now, note: "")
        loaded = draft
        foodsText = draft.foodsText
        amount = draft.amount
        when = draft.when
        note = draft.note
        isGone = component == nil
    }

    /// Now, or the entry's own time if it is somehow already ahead of the
    /// clock, so opening such an entry to fix its foods stays saveable.
    public var latestAllowedDate: Date { max(store.now, loaded.when) }

    public var canSave: Bool {
        !isGone && SolidsSheetCopy.canSave(foodsText: foodsText) && when <= latestAllowedDate
    }

    /// One `.update`. Moving the time moves the end with it, so the entry
    /// stays the moment it was logged as. A refused write puts the feed back
    /// as it was, so the edit never sits unsaved in the context for an
    /// unrelated save to commit without its change-log entry.
    public func save() throws {
        guard let (feed, index) = located() else {
            isGone = true
            throw EditModeError.gone
        }
        guard canSave, let componentID else { throw EditModeError.invalid }
        let before = (feed.components, feed.startedAt, feed.endedAt, feed.note, feed.lastModifiedAt)
        let shift = when.timeIntervalSince(feed.startedAt)
        feed.components[index] = .solids(id: componentID, foods: SolidsSheetCopy.foods(from: foodsText), amount: amount)
        feed.startedAt = when
        if let ended = feed.endedAt { feed.endedAt = ended.addingTimeInterval(shift) }
        feed.note = EditModel.trimmedToNil(note)
        do {
            // A moved time can make these solids a feed's first part.
            try store.updateFeedPart(feed, for: babyID)
        } catch {
            (feed.components, feed.startedAt, feed.endedAt, feed.note, feed.lastModifiedAt) = before
            throw error
        }
        loaded = Self.draft(feed, feed.components[index])
    }

    /// Through `deleteFeedPart`, never `softDelete`: every feed delete goes
    /// the way that keeps sessions keyed by a live entry (ruling H-R11).
    public func delete() throws {
        guard Self.fetch(feedID, babyID: babyID, in: store) != nil else {
            isGone = true
            throw EditModeError.gone
        }
        try store.deleteFeedPart(feedID, for: babyID)
    }

    /// Re-reads the entry after any write, this phone's or the other's.
    public func reload() {
        guard let (feed, index) = located() else {
            isGone = true
            return
        }
        let fresh = Self.draft(feed, feed.components[index])
        EditModel.merge(&foodsText, loaded: loaded.foodsText, fresh: fresh.foodsText)
        EditModel.merge(&amount, loaded: loaded.amount, fresh: fresh.amount)
        EditModel.merge(&when, loaded: loaded.when, fresh: fresh.when)
        EditModel.merge(&note, loaded: loaded.note, fresh: fresh.note)
        loaded = fresh
    }

    private func located() -> (FeedEntry, Int)? {
        guard let componentID, let feed = Self.fetch(feedID, babyID: babyID, in: store),
              let index = feed.components.firstIndex(where: { $0.id == componentID }) else { return nil }
        return (feed, index)
    }

    private static func draft(_ feed: FeedEntry, _ component: FeedComponent) -> Draft {
        Draft(foodsText: (component.solidFoods ?? []).joined(separator: ", "),
              amount: component.solidsAmount ?? .some, when: feed.startedAt, note: feed.note ?? "")
    }

    /// Only from the active realm: after a switch to or from a family, an
    /// entry of the other realm is gone as far as this sheet is concerned.
    private static func fetch(_ id: UUID, babyID: UUID, in store: BabyStore) -> FeedEntry? {
        guard case .feed(let feed)? = try? store.entry(id: id, for: babyID),
              feed.realmKey == store.activeRealm.encodedKey else { return nil }
        return feed
    }
}

/// New mode's state and its one write: the foods, amount, time and note a
/// solids entry is logged with — on their own, or as a part of a feed from
/// its session sheet's Add part.
@MainActor @Observable
public final class SolidsLogModel: Identifiable {
    private let store: BabyStore
    private let babyID: UUID
    /// The feed session Add part adds to; nil for solids on their own.
    private let session: UUID?

    public var foodsText = ""
    public var amount: SolidsAmount = .some
    public var note = ""
    /// Solids are logged after the fact as often as during, so the past —
    /// yesterday included — is open and the future is not.
    public var when: Date

    public init(store: BabyStore, babyID: UUID, session: UUID? = nil, at when: Date? = nil) {
        self.store = store
        self.babyID = babyID
        self.session = session
        self.when = when ?? store.now
    }

    public var latestAllowedDate: Date { store.now }

    public var canSave: Bool { SolidsSheetCopy.canSave(foodsText: foodsText) && when <= latestAllowedDate }

    /// One `.create`, note included; linked to the feed session when added
    /// to one.
    public func log() throws {
        guard canSave else { throw EditModeError.invalid }
        let foods = SolidsSheetCopy.foods(from: foodsText)
        guard let session else {
            _ = try store.logSolids(foods: foods, amount: amount, for: babyID, at: when,
                                    note: EditModel.trimmedToNil(note))
            return
        }
        try store.addFeedPart(.solids(foods: foods, amount: amount, at: when, note: EditModel.trimmedToNil(note)),
                              toSession: session, for: babyID)
    }
}

/// Solids have no timer and no volume, so the panel is a description, a
/// time and a note. `foods` is free text rather than a curated list: what a
/// baby eats is not a set we can enumerate. Logging and editing are the same
/// panel; editing adds Delete.
public struct SolidsSheet: View {
    let store: BabyStore
    let babyID: UUID
    let onDone: () -> Void
    /// From Past entry: Back returns to its list.
    private let onBack: (() -> Void)?

    @State private var logModel: SolidsLogModel
    /// Present in edit mode only.
    @State private var editModel: SolidsEditModel?
    @State private var isShowingDetails: Bool
    /// `onDone` runs once even when a delete is followed by the revision
    /// bump that marks the entry gone.
    @State private var isFinished = false
    @Environment(\.errorReporter) private var errorReporter

    public init(store: BabyStore, babyID: UUID, editing feedID: UUID? = nil, adding logModel: SolidsLogModel? = nil,
                onBack: (() -> Void)? = nil, onDone: @escaping () -> Void) {
        self.store = store
        self.babyID = babyID
        self.onDone = onDone
        self.onBack = onBack
        let editModel = feedID.map { SolidsEditModel(store: store, babyID: babyID, feedID: $0) }
        _logModel = State(wrappedValue: logModel ?? SolidsLogModel(store: store, babyID: babyID))
        _editModel = State(wrappedValue: editModel)
        _isShowingDetails = State(wrappedValue: !(editModel?.note.isEmpty ?? true))
    }

    private var canSave: Bool { editModel?.canSave ?? logModel.canSave }

    public var body: some View {
        EntryPanel(title: SolidsSheetCopy.title, icon: .solids, tint: Palette.solids, canSave: canSave,
                       identifierPrefix: "solids", onCancel: onBack ?? onDone,
                       cancelTitle: onBack == nil ? EditChromeCopy.cancel : EditChromeCopy.back,
                       onSave: save, onDelete: deleteAction) {
                CardSection {
                    TextField(SolidsSheetCopy.foodsLabel, text: foodsText, prompt: Text(SolidsSheetCopy.foodsPrompt))
                        .accessibilityLabel(SolidsSheetCopy.foodsLabel)
                        .accessibilityIdentifier("solids.foods")
                } footer: {
                    Text(
                        SolidsSheetCopy.canSave(foodsText: foodsText.wrappedValue)
                            ? SolidsSheetCopy.foodsFooter
                            : SolidsSheetCopy.foodsRequiredFooter
                    )
                }

                // No card behind a segmented control: it is a container already.
                Picker(SolidsSheetCopy.amountLabel, selection: amount) {
                    ForEach(SolidsAmount.allCases, id: \.self) { candidate in
                        Text(candidate.displayName).tag(candidate)
                    }
                }
                .pickerStyle(.segmented)

                CardSection {
                    EditTimeRow(SolidsSheetCopy.dateTimeLabel, date: when,
                                latest: editModel?.latestAllowedDate ?? logModel.latestAllowedDate,
                                identifier: "solids.time")
                }

                DetailsSection(isExpanded: $isShowingDetails, identifier: "solids.details") {
                    DetailFieldRow(EditChromeCopy.note, text: note, identifier: "solids.note")
                }
            }
            .editLifecycle(isPresented: editModel != nil, revision: store.revision,
                           isGone: editModel?.isGone ?? false, reload: { editModel?.reload() }, onGone: finish)
    }

    // MARK: - Fields: the edit model's in edit mode, the log model's otherwise

    private var foodsText: Binding<String> {
        if let editModel { Bindable(editModel).foodsText } else { $logModel.foodsText }
    }

    private var amount: Binding<SolidsAmount> {
        if let editModel { Bindable(editModel).amount } else { $logModel.amount }
    }

    private var when: Binding<Date> {
        if let editModel { Bindable(editModel).when } else { $logModel.when }
    }

    private var note: Binding<String> {
        if let editModel { Bindable(editModel).note } else { $logModel.note }
    }

    private func save() {
        if editModel == nil { log() } else { saveEdit() }
    }

    /// Delete only in edit mode.
    private var deleteAction: (() -> Void)? {
        guard editModel != nil else { return nil }
        return delete
    }

    private func log() {
        do {
            try logModel.log()
            errorReporter.clear()
            onDone()
        } catch {
            // Stays open with the typed foods intact, so a
            // retry costs one tap rather than re-typing.
            errorReporter.report(
                ErrorBannerCopy.saveFailed(what: ErrorSubject.solids),
                error: error,
                category: AppLog.store
            )
        }
    }

    // MARK: - Edit mode

    private func saveEdit() {
        guard let editModel else { return }
        if performEdit(editModel.save, failure: ErrorBannerCopy.saveFailed(what: ErrorSubject.solids),
                       reporter: errorReporter) { finish() }
    }

    private func delete() {
        guard let editModel else { return }
        if performEdit(editModel.delete, failure: ErrorBannerCopy.deleteFailed(what: ErrorSubject.solids),
                       reporter: errorReporter) { finish() }
    }

    private func finish() {
        guard !isFinished else { return }
        isFinished = true
        onDone()
    }
}
