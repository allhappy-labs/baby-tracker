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
    public static let timeLabel = LoggingL10n.text("Time")
    public static let cancel = LoggingL10n.text("Cancel")
    public static let save = LoggingL10n.text("Save")
    public static let dateTimeLabel = LoggingL10n.text("Date and time")
    public static let noteLabel = LoggingL10n.text("Note")
    public static let notePrompt = LoggingL10n.text("Optional")

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
            try store.update(feed)
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

/// Solids have no timer and no volume, so the sheet is a description and a
/// time. `foods` is free text rather than a curated list: what a baby eats is
/// not a set we can enumerate.
public struct SolidsSheet: View {
    let store: BabyStore
    let babyID: UUID
    let onDone: () -> Void

    @State private var foodsText: String = ""
    @State private var amount: SolidsAmount = .some
    @State private var when: Date
    /// Present in edit mode only. Create mode keeps its own `@State` above
    /// and never touches a model.
    @State private var editModel: SolidsEditModel?
    /// `onDone` runs once even when a delete is followed by the revision
    /// bump that marks the entry gone.
    @State private var isFinished = false
    @Environment(\.errorReporter) private var errorReporter

    private var canSave: Bool { editModel?.canSave ?? SolidsSheetCopy.canSave(foodsText: foodsText) }

    /// Edit mode (`editing:` a solids feed's id) is the same form with the
    /// entry's values: its time with the date at the top, a note, and Save
    /// and Delete at the bottom.
    public init(store: BabyStore, babyID: UUID, editing feedID: UUID? = nil, onDone: @escaping () -> Void) {
        self.store = store
        self.babyID = babyID
        self.onDone = onDone
        _when = State(wrappedValue: store.now)
        _editModel = State(wrappedValue: feedID.map { SolidsEditModel(store: store, babyID: babyID, feedID: $0) })
    }

    public var body: some View {
        NavigationStack {
            Form {
                // Edit mode's title row: the entry's own time, with its date.
                if let editModel {
                    Section {
                        EditTimeRow(
                            SolidsSheetCopy.dateTimeLabel,
                            date: Binding(get: { editModel.when }, set: { editModel.when = $0 }),
                            latest: editModel.latestAllowedDate,
                            identifier: "solids.time"
                        )
                    }
                }

                Section {
                    TextField(
                        SolidsSheetCopy.foodsLabel,
                        text: field(\.foodsText, else: $foodsText),
                        prompt: Text(SolidsSheetCopy.foodsPrompt)
                    )
                    .accessibilityLabel(SolidsSheetCopy.foodsLabel)
                    .accessibilityIdentifier("solids.foods")
                } footer: {
                    Text(
                        SolidsSheetCopy.canSave(foodsText: editModel?.foodsText ?? foodsText)
                            ? SolidsSheetCopy.foodsFooter
                            : SolidsSheetCopy.foodsRequiredFooter
                    )
                }

                Section {
                    Picker(SolidsSheetCopy.amountLabel, selection: amountBinding) {
                        ForEach(SolidsAmount.allCases, id: \.self) { candidate in
                            Text(candidate.displayName).tag(candidate)
                        }
                    }
                    .pickerStyle(.segmented)

                    if editModel == nil {
                        // Solids are logged after the fact as often as during, so
                        // the past is open and the future is not.
                        DatePicker(
                            SolidsSheetCopy.timeLabel,
                            selection: $when,
                            in: ...store.now,
                            displayedComponents: [.hourAndMinute]
                        )
                    }
                }

                if editModel != nil {
                    // The old editor's note, so editing solids here loses nothing.
                    Section {
                        TextField(SolidsSheetCopy.noteLabel, text: field(\.note, else: .constant("")),
                                  prompt: Text(SolidsSheetCopy.notePrompt))
                            .accessibilityLabel(SolidsSheetCopy.noteLabel)
                            .accessibilityIdentifier("solids.note")
                    }
                }
            }
            .editFooter(isPresented: editModel != nil, canSave: canSave, identifierPrefix: "solids",
                        onSave: saveEdit, onDelete: delete)
            // Sized to the form. `.medium` left a third of the sheet empty
            // under the time row. Edit mode adds a note row and the footer.
            .sheetHeight(editModel == nil ? 340 : 400 + EditFooter.height)
            .navigationTitle(SolidsSheetCopy.title)
            .editTitle(isPresented: editModel != nil, icon: .solids, tint: Palette.solids, label: SolidsSheetCopy.title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(SolidsSheetCopy.cancel, action: onDone)
                }
                if editModel == nil {
                    ToolbarItem(placement: .confirmationAction) {
                        Button(SolidsSheetCopy.save, action: log)
                            .buttonStyle(.borderedProminent)
                            .tint(Palette.feed)
                            .disabled(!canSave)
                    }
                }
            }
            .editLifecycle(isPresented: editModel != nil, revision: store.revision,
                           isGone: editModel?.isGone ?? false, reload: { editModel?.reload() }, onGone: finish)
        }
    }

    /// The edit model's field in edit mode, the sheet's own state otherwise.
    private func field(_ keyPath: ReferenceWritableKeyPath<SolidsEditModel, String>, else own: Binding<String>) -> Binding<String> {
        guard let editModel else { return own }
        return Binding(get: { editModel[keyPath: keyPath] }, set: { editModel[keyPath: keyPath] = $0 })
    }

    private var amountBinding: Binding<SolidsAmount> {
        guard let editModel else { return $amount }
        return Binding(get: { editModel.amount }, set: { editModel.amount = $0 })
    }

    private func log() {
        do {
            _ = try store.logSolids(
                foods: SolidsSheetCopy.foods(from: foodsText),
                amount: amount,
                for: babyID,
                at: when
            )
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
