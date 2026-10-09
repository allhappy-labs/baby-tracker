import SwiftUI
import Observation
import BabyCore
import BabyProtocol
import BabyStore
import BabyUI

public enum SupplementSheetCopy {
    public static let title = LoggingL10n.text("Meds")
    public static let addName = LoggingL10n.text("Add med")
    public static let name = LoggingL10n.text("Med name")
    public static let invitation = LoggingL10n.text("Add your first med")
    public static let validation = LoggingL10n.text("Enter a name of up to 64 characters, without control characters.")
    public static let log = LoggingL10n.text("Log")
    public static let time = LoggingL10n.text("Date and time")
    public static let note = LoggingL10n.text("Note")
    public static let notePrompt = LoggingL10n.text("Optional")
}

/// Logs a med, or in edit mode (`editing:` an entry's id) renames it, moves
/// its time, edits its note and deletes it.
///
/// Edit mode holds the entry's id, not the entry: the other phone may change
/// or delete it while the sheet is open. Every write fetches it afresh, and
/// `reload()` — called on every `store.revision` — notices when it has gone.
@MainActor @Observable
public final class SupplementSheetModel {
    private let store: BabyStore
    private let babyID: UUID
    private var writeContext: RealmWriteContext?
    /// The entry edit mode is editing; nil when logging.
    public let editingID: UUID?
    public private(set) var names: [String] = []
    public private(set) var isAddingName = false
    public private(set) var errorMessage: String?
    public var selectedName = ""
    public var newName = ""
    public var when: Date
    /// Edit mode only: the entry's note. Logging has no note field.
    public var note = ""
    public let latestAllowedDate: Date
    /// Edit mode only: true once the entry has vanished. The sheet closes on it.
    public private(set) var isGone = false
    /// The entry's fields as last read, so `reload()` takes the other phone's
    /// change only into fields the user has not touched.
    private var loaded: Loaded?

    private struct Loaded: Equatable {
        var name: String
        var when: Date
        var note: String
    }

    public init(store: BabyStore, babyID: UUID, editing entryID: UUID? = nil) {
        self.store = store
        self.babyID = babyID
        editingID = entryID
        let entry = entryID.flatMap { Self.fetch($0, babyID: babyID, in: store) }
        when = entry?.startedAt ?? store.now
        latestAllowedDate = max(store.now, entry?.startedAt ?? store.now)
        do {
            writeContext = try store.captureWriteContext()
            names = try store.supplementNames(for: babyID)
            if let entry {
                let loaded = Loaded(name: Self.listed(entry.name, in: names), when: entry.startedAt, note: entry.note ?? "")
                self.loaded = loaded
                selectedName = loaded.name
                note = loaded.note
                isAddingName = false
            } else {
                selectedName = names.first ?? ""
                isAddingName = names.isEmpty
            }
        } catch {
            errorMessage = LoggingL10n.text("Couldn’t load meds. Close this sheet and try again.")
        }
        isGone = entryID != nil && entry == nil
    }

    public var isEditing: Bool { editingID != nil }
    public var showsPicker: Bool { names.count > 1 }
    public var canLog: Bool {
        guard writeContext != nil, !isGone,
              (try? SupplementName.normalized(isAddingName ? newName : selectedName)) != nil else { return false }
        // Logging's time picker already stops at now; an edit is also
        // checked here, because `save()` refuses what this refuses.
        return !isEditing || when <= latestAllowedDate
    }
    public func addName() { isAddingName = true }
    public func useSavedNames() { isAddingName = false }

    /// Edit mode's write: one `.update` of name, time and note. Refuses in
    /// create mode, where `log()` is the write.
    public func save() throws {
        guard let editingID else { throw EditModeError.invalid }
        guard let entry = Self.fetch(editingID, babyID: babyID, in: store) else {
            isGone = true
            throw EditModeError.gone
        }
        guard canLog, let writeContext else { throw EditModeError.invalid }
        do {
            try store.editSupplement(entry, name: isAddingName ? newName : selectedName, at: when,
                                     note: EditModel.trimmedToNil(note), expectedWriteContext: writeContext)
            errorMessage = nil
        } catch {
            errorMessage = LoggingL10n.text("Couldn’t save med. Your entry is still here; try again.")
            throw error
        }
        reload()
    }

    /// A tombstone. Meds are not in sessions, so the plain `softDelete` is
    /// the right call (ruling H-R11). A refused delete puts the flag back.
    public func delete() throws {
        guard let editingID else { throw EditModeError.invalid }
        guard let entry = Self.fetch(editingID, babyID: babyID, in: store) else {
            isGone = true
            throw EditModeError.gone
        }
        let modifiedAt = entry.lastModifiedAt
        do {
            try store.softDelete(entry)
        } catch {
            entry.isTombstoned = false
            entry.lastModifiedAt = modifiedAt
            throw error
        }
    }

    /// Re-reads the entry after any write, this phone's or the other's.
    /// Does nothing in create mode.
    public func reload() {
        guard let editingID else { return }
        guard let entry = Self.fetch(editingID, babyID: babyID, in: store) else {
            isGone = true
            return
        }
        names = (try? store.supplementNames(for: babyID)) ?? names
        let fresh = Loaded(name: Self.listed(entry.name, in: names), when: entry.startedAt, note: entry.note ?? "")
        if let loaded {
            if !isAddingName { EditModel.merge(&selectedName, loaded: loaded.name, fresh: fresh.name) }
            EditModel.merge(&when, loaded: loaded.when, fresh: fresh.when)
            EditModel.merge(&note, loaded: loaded.note, fresh: fresh.note)
        }
        loaded = fresh
    }

    /// The picker's spelling of `name`: the list keeps the first spelling a
    /// med was logged with, and a tag that matches none of its rows shows blank.
    private static func listed(_ name: String, in names: [String]) -> String {
        names.first { $0.caseInsensitiveCompare(name) == .orderedSame } ?? name
    }

    private static func fetch(_ id: UUID, babyID: UUID, in store: BabyStore) -> SupplementEntry? {
        guard case .supplement(let entry)? = try? store.entry(id: id, for: babyID) else { return nil }
        return entry
    }

    /// The view dismisses only after this single atomic write succeeds.
    public func log() -> Bool {
        guard canLog, let writeContext else { return false }
        do {
            try store.logSupplement(name: isAddingName ? newName : selectedName,
                                    for: babyID, at: when, expectedWriteContext: writeContext)
            errorMessage = nil
            return true
        } catch {
            errorMessage = LoggingL10n.text("Couldn’t save med. Your entry is still here; try again.")
            return false
        }
    }
}

public struct SupplementSheet: View {
    @State private var model: SupplementSheetModel
    private let store: BabyStore
    private let onDone: () -> Void
    @FocusState private var nameFocused: Bool
    @Environment(\.errorReporter) private var errorReporter
    /// `onDone` runs once even when a delete is followed by the revision
    /// bump that marks the entry gone.
    @State private var isFinished = false

    public init(store: BabyStore, babyID: UUID, editing entryID: UUID? = nil, onDone: @escaping () -> Void) {
        _model = State(initialValue: SupplementSheetModel(store: store, babyID: babyID, editing: entryID))
        self.store = store
        self.onDone = onDone
    }

    public var body: some View {
        NavigationStack {
            Form {
                // Edit mode's title row: the entry's own time, with its date.
                if model.isEditing {
                    Section {
                        EditTimeRow(SupplementSheetCopy.time, date: $model.when,
                                    latest: model.latestAllowedDate, identifier: "supplement.time")
                    }
                }
                Section {
                    if model.isAddingName {
                        if model.names.isEmpty { Text(SupplementSheetCopy.invitation) }
                        TextField(SupplementSheetCopy.name, text: $model.newName)
                            .accessibilityLabel(SupplementSheetCopy.name)
                            .accessibilityIdentifier("supplement.name")
                            .focused($nameFocused)
                        if !model.names.isEmpty {
                            Button(LoggingL10n.text("Use saved meds")) { model.useSavedNames() }
                        }
                    } else if model.showsPicker {
                        Picker(LoggingL10n.text("Med"), selection: $model.selectedName) {
                            ForEach(model.names, id: \.self) { Text($0).tag($0) }
                        }
                        .accessibilityIdentifier("supplement.picker")
                    } else {
                        Text(model.selectedName).accessibilityIdentifier("supplement.selectedName")
                    }
                } footer: {
                    if model.isAddingName && !model.canLog { Text(SupplementSheetCopy.validation) }
                }
                if model.isEditing {
                    // The old editor's note, so editing a med here loses nothing.
                    Section {
                        TextField(SupplementSheetCopy.note, text: $model.note,
                                  prompt: Text(SupplementSheetCopy.notePrompt))
                            .accessibilityLabel(SupplementSheetCopy.note)
                            .accessibilityIdentifier("supplement.note")
                    }
                } else {
                    Section {
                        DatePicker(SupplementSheetCopy.time, selection: $model.when,
                                   in: ...model.latestAllowedDate, displayedComponents: [.date, .hourAndMinute])
                            .accessibilityIdentifier("supplement.time")
                        Button(SupplementSheetCopy.log) {
                            if model.log() { onDone() }
                        }
                        .disabled(!model.canLog)
                        .accessibilityIdentifier("supplement.log")
                    }
                }
                if let error = model.errorMessage {
                    Section { Text(error).foregroundStyle(.red).accessibilityIdentifier("supplement.error") }
                }
            }
            .editFooter(isPresented: model.isEditing, canSave: model.canLog, identifierPrefix: "supplement",
                        onSave: save, onDelete: delete)
            .navigationTitle(SupplementSheetCopy.title)
            .editTitle(isPresented: model.isEditing, icon: .meds, tint: Palette.meds, label: SupplementSheetCopy.title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button(LoggingL10n.text("Cancel"), action: onDone) }
                ToolbarItem(placement: .primaryAction) {
                    Button { model.addName(); nameFocused = true } label: { Image(systemName: "plus") }
                        .accessibilityLabel(SupplementSheetCopy.addName)
                        .accessibilityIdentifier("supplement.add")
                }
            }
            .editLifecycle(isPresented: model.isEditing, revision: store.revision, isGone: model.isGone,
                           reload: model.reload, onGone: finish)
        }
    }

    // MARK: - Edit mode

    /// No banner: a failed save shows the model's own message in the form,
    /// as logging does. `.invalid` sets none, and needs none — Save is off
    /// whenever `canLog` is false, which is exactly when `save()` refuses.
    private func save() {
        if performEdit(model.save, failure: nil, reporter: errorReporter) { finish() }
    }

    private func delete() {
        if performEdit(model.delete, failure: ErrorBannerCopy.deleteFailed(what: ErrorSubject.supplement),
                       reporter: errorReporter) { finish() }
    }

    private func finish() {
        guard !isFinished else { return }
        isFinished = true
        onDone()
    }
}
