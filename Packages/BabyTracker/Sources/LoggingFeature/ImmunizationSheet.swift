import SwiftUI
import Observation
import BabyCore
import BabyProtocol
import BabyStore
import BabyUI

public enum ImmunizationSheetCopy {
    public static let addName = LoggingL10n.text("Add vaccine")
    public static let name = LoggingL10n.text("Vaccine name")
    public static let invitation = LoggingL10n.text("Add your first vaccine")
    public static let vaccine = LoggingL10n.text("Vaccine")
    public static let choose = LoggingL10n.text("Choose vaccine")
    public static let useSaved = LoggingL10n.text("Use saved vaccines")
    public static let validation = LoggingL10n.text("Enter a name of up to 64 characters, without control characters.")
    public static let dose = LoggingL10n.text("Dose")
    public static let noDose = LoggingL10n.text("No dose")
    public static let decreaseDose = LoggingL10n.text("Decrease dose")
    public static let increaseDose = LoggingL10n.text("Increase dose")
    public static let beforeBirth = LoggingL10n.text("Choose a date on or after the baby’s birth date.")
    public static let inTheFuture = LoggingL10n.text("Choose a date and time that is not in the future.")
    public static let loadFailed = LoggingL10n.text("Couldn’t load vaccines. Close this sheet and try again.")
    public static let saveFailed = LoggingL10n.text("Couldn’t save vaccination. Your entry is still here; try again.")
}

/// One vaccination, logged or edited. The dose follows the chosen vaccine —
/// one more than this baby's highest — until the caregiver sets it by hand;
/// when editing it is the stored dose and never follows.
@MainActor @Observable
public final class ImmunizationSheetModel {
    private let store: BabyStore
    private let babyID: UUID
    private var writeContext: RealmWriteContext?
    public let editingID: UUID?
    public private(set) var names: [String] = []
    public private(set) var isAddingName = false
    public private(set) var errorMessage: String?
    /// The picker's choice; nil until the caregiver chooses.
    public private(set) var selectedName: String?
    public private(set) var newName = ""
    public private(set) var dose: Int?
    public var when: Date
    public var note = ""
    public let timeZoneID: String
    public let latestAllowedDate: Date
    public private(set) var isGone = false
    private var doseFollowsName: Bool
    private var loaded: Loaded?
    private let birthDay: LocalDate?
    /// When the panel opened, and the time the entry had then (edit mode).
    private let openedAt: Date
    private let originalWhen: Date?

    private struct Loaded: Equatable {
        var name: String
        var dose: Int?
        var when: Date
        var note: String
    }

    public init(store: BabyStore, babyID: UUID, editing entryID: UUID? = nil) {
        self.store = store
        self.babyID = babyID
        editingID = entryID
        let entry = entryID.flatMap { Self.fetch($0, babyID: babyID, in: store) }
        when = entry?.givenAt ?? store.now
        timeZoneID = entry?.timeZoneID ?? TimeZone.current.identifier
        latestAllowedDate = max(store.now, entry?.givenAt ?? store.now)
        openedAt = store.now
        originalWhen = entry?.givenAt
        doseFollowsName = entryID == nil
        let baby = try? store.baby(babyID)
        birthDay = baby?.localBirthDate ?? baby.flatMap { try? LocalDate(displaying: $0.birthDate, in: .current) }
        do {
            writeContext = try store.captureWriteContext()
            names = try store.vaccineNames()
            if let entry {
                let loaded = Loaded(name: Self.listed(entry.vaccine, in: names), dose: entry.dose,
                                    when: entry.givenAt, note: entry.note ?? "")
                self.loaded = loaded
                selectedName = loaded.name
                dose = loaded.dose
                note = loaded.note
            } else {
                isAddingName = names.isEmpty
            }
        } catch {
            errorMessage = ImmunizationSheetCopy.loadFailed
        }
        isGone = entryID != nil && entry == nil
    }

    public var isEditing: Bool { editingID != nil }
    /// The name Save would write, before normalising.
    public var vaccine: String? { isAddingName ? newName : selectedName }
    public var hasValidName: Bool { vaccine.flatMap { try? SupplementName.normalized($0) } != nil }

    /// Growth's rule, in the entry's zone; nil when the time is fine.
    public var dateMessage: String? {
        // A time already in this phone's future (the other phone's clock ran
        // fast) may stay where it is, as the store allows; it may not be moved
        // to another future moment.
        if when > latestAllowedDate || (when > openedAt && when != originalWhen) {
            return ImmunizationSheetCopy.inTheFuture
        }
        guard let birthDay, let zone = TimeZone(identifier: timeZoneID),
              let day = try? LocalDate(displaying: when, in: zone) else { return nil }
        return day < birthDay ? ImmunizationSheetCopy.beforeBirth : nil
    }

    public var canSave: Bool { writeContext != nil && !isGone && hasValidName && dateMessage == nil }

    public func addName() { isAddingName = true; follow() }
    public func useSavedNames() { isAddingName = false; follow() }
    public func choose(_ name: String?) { selectedName = name; follow() }
    public func type(_ text: String) { newName = text; follow() }

    /// Takes in names another phone's shots brought while the panel is open.
    /// What is being typed is kept; a selection whose vaccine is gone is
    /// cleared rather than left pointing at nothing.
    public func refreshNames() {
        guard let fresh = try? store.vaccineNames(), fresh != names else { return }
        names = fresh
        if editingID == nil {
            if let selected = selectedName {
                let key = BabyStore.vaccineNameKey(selected)
                selectedName = fresh.first { BabyStore.vaccineNameKey($0) == key }
            }
            if fresh.isEmpty { isAddingName = true }
            follow()
        }
    }

    /// − below 1 is no dose; + from no dose is 1; never above 10.
    public func stepDose(_ direction: Int) {
        doseFollowsName = false
        let next = (dose ?? 0) + direction
        dose = next < BabyStore.immunizationDoses.lowerBound ? nil : min(next, BabyStore.immunizationDoses.upperBound)
    }

    private func follow() {
        guard doseFollowsName else { return }
        dose = hasValidName ? vaccine.flatMap { try? store.nextDose(of: $0, for: babyID) } : nil
    }

    /// The view dismisses only after this single atomic write succeeds.
    public func log() -> Bool {
        guard editingID == nil, canSave, let writeContext, let vaccine else { return false }
        do {
            try store.logImmunization(vaccine: vaccine, dose: dose, for: babyID, at: when,
                                      note: EditModel.trimmedToNil(note), expectedWriteContext: writeContext)
            errorMessage = nil
            return true
        } catch {
            errorMessage = ImmunizationSheetCopy.saveFailed
            return false
        }
    }

    public func save() throws {
        guard let editingID else { throw EditModeError.invalid }
        guard let entry = Self.fetch(editingID, babyID: babyID, in: store) else {
            isGone = true
            throw EditModeError.gone
        }
        guard canSave, let writeContext, let vaccine else { throw EditModeError.invalid }
        do {
            try store.editImmunization(entry, vaccine: vaccine, dose: dose, at: when,
                                       note: EditModel.trimmedToNil(note), expectedWriteContext: writeContext)
            errorMessage = nil
        } catch {
            errorMessage = ImmunizationSheetCopy.saveFailed
            throw error
        }
        reload()
    }

    /// A tombstone. A refused delete puts the flag back.
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
    public func reload() {
        guard let editingID else { return }
        guard let entry = Self.fetch(editingID, babyID: babyID, in: store) else {
            isGone = true
            return
        }
        names = (try? store.vaccineNames()) ?? names
        let fresh = Loaded(name: Self.listed(entry.vaccine, in: names), dose: entry.dose,
                           when: entry.givenAt, note: entry.note ?? "")
        if let loaded {
            if !isAddingName, var name = selectedName {
                EditModel.merge(&name, loaded: loaded.name, fresh: fresh.name)
                selectedName = name
            }
            EditModel.merge(&dose, loaded: loaded.dose, fresh: fresh.dose)
            EditModel.merge(&when, loaded: loaded.when, fresh: fresh.when)
            EditModel.merge(&note, loaded: loaded.note, fresh: fresh.note)
        }
        loaded = fresh
    }

    private static func listed(_ name: String, in names: [String]) -> String {
        names.first { BabyStore.vaccineNameKey($0) == BabyStore.vaccineNameKey(name) } ?? name
    }

    private static func fetch(_ id: UUID, babyID: UUID, in store: BabyStore) -> ImmunizationEntry? {
        guard case .immunization(let entry)? = try? store.entry(id: id, for: babyID) else { return nil }
        return entry
    }
}

public struct ImmunizationSheet: View {
    @State private var model: ImmunizationSheetModel
    private let store: BabyStore
    private let onDone: () -> Void
    @FocusState private var nameFocused: Bool
    @Environment(\.errorReporter) private var errorReporter
    @Environment(\.dynamicTypeSize) private var typeSize
    @State private var isFinished = false
    @State private var isShowingDetails: Bool

    public init(store: BabyStore, babyID: UUID, editing entryID: UUID? = nil, onDone: @escaping () -> Void) {
        let model = ImmunizationSheetModel(store: store, babyID: babyID, editing: entryID)
        _model = State(initialValue: model)
        _isShowingDetails = State(initialValue: !model.note.isEmpty)
        self.store = store
        self.onDone = onDone
    }

    public var body: some View {
        EntryPanel(title: Tracker.vaccine.title, icon: .vaccine, tint: Palette.vaccine, canSave: model.canSave,
                   identifierPrefix: "immunization", onCancel: onDone,
                   onSave: saveOrLog, onDelete: deleteAction,
                   accessory: model.names.isEmpty ? nil : AnyView(addNameButton)) {
            CardSection {
                if model.isAddingName {
                    if model.names.isEmpty { Text(ImmunizationSheetCopy.invitation) }
                    TextField(ImmunizationSheetCopy.name, text: Binding(get: { model.newName }, set: { model.type($0) }))
                        .accessibilityLabel(ImmunizationSheetCopy.name)
                        .accessibilityIdentifier("immunization.name")
                        .focused($nameFocused)
                    if !model.names.isEmpty {
                        Button(ImmunizationSheetCopy.useSaved) { model.useSavedNames() }
                    }
                } else {
                    vaccineRow
                }
                doseRow
            } footer: {
                if model.isAddingName && !model.newName.isEmpty && !model.hasValidName {
                    Text(ImmunizationSheetCopy.validation)
                }
            }
            CardSection {
                EditTimeRow(date: $model.when, latest: model.latestAllowedDate, identifier: "immunization.time")
                    .environment(\.timeZone, TimeZone(identifier: model.timeZoneID) ?? .current)
                if let message = model.dateMessage {
                    InlineError(message).accessibilityIdentifier("immunization.validation.date")
                }
            }
            DetailsSection(isExpanded: $isShowingDetails, identifier: "immunization.details") {
                DetailFieldRow(EditChromeCopy.note, text: $model.note, identifier: "immunization.note")
            }
            if let error = model.errorMessage {
                InlineError(error).accessibilityIdentifier("immunization.error")
            }
        }
        .editLifecycle(isPresented: model.isEditing, revision: store.revision, isGone: model.isGone,
                       reload: model.reload, onGone: finish)
        // Edit mode re-reads through `reload`; a new entry only needs the names.
        .onChange(of: store.revision) { if !model.isEditing { model.refreshNames() } }
    }

    /// The row names the picker; the menu's label is drawn here, because the
    /// system menu picker's own label reports one line of height while its
    /// text wraps to two at the accessibility sizes, and ran into the row
    /// below. There the name sits above the menu, each on its own line.
    @ViewBuilder private var vaccineRow: some View {
        let menu = Menu {
            Picker(ImmunizationSheetCopy.vaccine,
                   selection: Binding(get: { model.selectedName }, set: { model.choose($0) })) {
                Text(ImmunizationSheetCopy.choose).tag(String?.none)
                ForEach(model.names, id: \.self) { Text($0).tag(Optional($0)) }
            }
        } label: {
            HStack(spacing: 6) {
                Text(model.selectedName ?? ImmunizationSheetCopy.choose)
                    .multilineTextAlignment(typeSize.isAccessibilitySize ? .leading : .trailing)
                    .fixedSize(horizontal: false, vertical: true)
                Image(systemName: "chevron.up.chevron.down").font(.footnote.weight(.semibold))
            }
            .frame(minHeight: 44)
            .contentShape(Rectangle())
        }
        .accessibilityLabel(ImmunizationSheetCopy.vaccine)
        .accessibilityValue(model.selectedName ?? ImmunizationSheetCopy.choose)
        .accessibilityIdentifier("immunization.picker")
        if typeSize.isAccessibilitySize {
            VStack(alignment: .leading, spacing: 4) {
                Text(ImmunizationSheetCopy.vaccine)
                menu
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        } else {
            HStack(spacing: 8) {
                Text(ImmunizationSheetCopy.vaccine)
                Spacer(minLength: 8)
                menu
            }
        }
    }

    /// "Dose  −  2  +". At the accessibility sizes the value sits above the two buttons.
    @ViewBuilder private var doseRow: some View {
        let value = Text(model.dose.map { "\($0)" } ?? "—")
            .font(.body.monospacedDigit())
            .frame(minWidth: 32)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(ImmunizationSheetCopy.dose)
            .accessibilityValue(model.dose.map { "\($0)" } ?? ImmunizationSheetCopy.noDose)
            .accessibilityIdentifier("immunization.dose")
        if typeSize.isAccessibilitySize {
            VStack(alignment: .leading, spacing: 8) {
                HStack { Text(ImmunizationSheetCopy.dose); Spacer(minLength: 8); value }
                HStack(spacing: 8) { minus; plus }
            }
        } else {
            HStack(spacing: 8) {
                Text(ImmunizationSheetCopy.dose)
                Spacer(minLength: 8)
                minus.frame(width: 56)
                value
                plus.frame(width: 56)
            }
        }
    }

    private var minus: some View {
        stepButton(-1, systemImage: "minus", label: ImmunizationSheetCopy.decreaseDose, identifier: "immunization.dose.minus")
            .disabled(model.dose == nil)
    }

    private var plus: some View {
        stepButton(1, systemImage: "plus", label: ImmunizationSheetCopy.increaseDose, identifier: "immunization.dose.plus")
            .disabled(model.dose == BabyStore.immunizationDoses.upperBound)
    }

    private func stepButton(_ direction: Int, systemImage: String, label: String, identifier: String) -> some View {
        Button { model.stepDose(direction) } label: {
            Image(systemName: systemImage).frame(maxWidth: .infinity, minHeight: 44)
        }
        .buttonStyle(.tile(Palette.vaccine, size: .widget))
        .accessibilityLabel(label)
        .accessibilityIdentifier(identifier)
    }

    private var addNameButton: some View {
        CircleIconButton(systemImage: "plus", label: ImmunizationSheetCopy.addName, identifier: "immunization.add") {
            model.addName()
            nameFocused = true
        }
    }

    private func saveOrLog() {
        if model.isEditing { save() } else if model.log() { onDone() }
    }

    private var deleteAction: (() -> Void)? {
        guard model.isEditing else { return nil }
        return delete
    }

    private func save() {
        if performEdit(model.save, failure: nil, reporter: errorReporter) { finish() }
    }

    private func delete() {
        if performEdit(model.delete, failure: ErrorBannerCopy.deleteFailed(what: ErrorSubject.vaccination),
                       reporter: errorReporter) { finish() }
    }

    private func finish() {
        guard !isFinished else { return }
        isFinished = true
        onDone()
    }
}
