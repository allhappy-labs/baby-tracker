import SwiftUI
import BabyCore
import BabyStore
import BabyUI
import BabyFeature

/// Every string the edit sheet shows. Strings never live in a view: a string
/// baked into a `View` cannot be read by a test, and this project found
/// thirteen copy defects by reading rendered strings.
public enum BabyEditCopy {
    public static let profileLoadFailed = String(localized: "Growth settings could not be loaded. Saving will only update name and birth date.", bundle: .module)
    public static let retryGrowthSettings = String(localized: "Retry loading growth settings", bundle: .module)
    public static let referenceFooter = String(localized: "Choose a reference to enable growth comparisons. Existing measurements remain available without one.", bundle: .module)
    public static let title = String(localized: "Edit baby", bundle: .module)
    /// Shown to a caregiver instead of the control. It says who can do it, so
    /// the answer is "ask the owner" rather than "the app is broken".
    public static let deleteOwnerOnly = String(localized: "Only the family owner can remove a baby.", bundle: .module)
    /// The owner's footer in a shared family. Deleting is not local: it takes
    /// every caregiver's records with it, and any running timer.
    public static let sharedDeleteFooter = String(localized: "Removing a baby removes it for everyone in the family, along with every entry logged against it and any timer that is running. This cannot be undone from this device.", bundle: .module)
    public static let cancelLabel = String(localized: "Cancel", bundle: .module)
    public static let saveLabel = String(localized: "Save", bundle: .module)

    public static let deleteLabel = String(localized: "Delete baby", bundle: .module)
    /// Names the baby outright. "Delete this baby?" over a list of two is a
    /// question you cannot answer without closing the dialog to check.
    public static func deleteConfirmTitle(name: String) -> String {
        String(format: String(localized: "Delete %@?", bundle: .module), name)
    }
    public static let deleteConfirmAction = String(localized: "Delete", bundle: .module)
    public static let deleteKeepAction = String(localized: "Keep", bundle: .module)
    /// Says what delete actually does, because it is not what it sounds like:
    /// the entries survive, and a parent deciding whether to tap needs to know
    /// that before they tap rather than after.
    public static let deleteFooter =
        String(localized: "Removes them from the app. Nothing already logged is deleted.", bundle: .module)
}

/// Correcting a baby's name or birth date, and the only route to removing one.
public struct BabyEditSheet: View {
    private let store: BabyStore
    private let baby: Baby
    private let onDone: () -> Void
    private let onDeleted: (UUID) -> Void

    @State private var name: String
    @State private var birthDate: Date
    @State private var growth: BabyGrowthProfileEditor
    @State private var isConfirmingDelete = false
    @Environment(\.errorReporter) private var errorReporter

    public init(store: BabyStore, baby: Baby, onDone: @escaping () -> Void,
                onDeleted: @escaping (UUID) -> Void = { _ in }) {
        self.store = store
        self.baby = baby
        self.onDone = onDone
        self.onDeleted = onDeleted
        _name = State(wrappedValue: baby.name)
        _birthDate = State(wrappedValue: baby.birthDate)
        _growth = State(wrappedValue: BabyGrowthProfileEditor(fallbackDate: baby.birthDate))
    }

    private func loadGrowthProfile() {
        let wasFailed = growth.failed
        growth.load(fetch: { try store.growthProfile(for: baby.id) }) { error in
            errorReporter.report(BabyEditCopy.profileLoadFailed, error: error, category: AppLog.store)
        }
        if wasFailed && growth.loaded { errorReporter.clear() }
    }

    /// Mirrors `renameBaby`'s own guard, so the disabled Save and the thrown
    /// error can never disagree about what an empty name is.
    private var canSave: Bool {
        !name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    /// Whether this device's role permits removing a baby from the family.
    ///
    /// `store.revision` is read so a role change re-renders this sheet; the
    /// permission is a fetch, not an observable property.
    private var canDeleteBaby: Bool {
        _ = store.revision
        guard case .shared = store.activeRealm else { return true }
        return (try? store.writePermission())?.canManageMembers ?? false
    }

    /// Presented full height from Settings.
    public var body: some View {
        SheetScaffold(
            BabyEditCopy.title,
            closeTitle: BabyEditCopy.cancelLabel,
            onClose: onDone,
            primary: SheetAction(BabyEditCopy.saveLabel, identifier: "baby.save", isEnabled: canSave, action: save)
        ) {
            BabyFormFields(
                name: $name,
                birthDate: $birthDate,
                latestBirthDate: store.now,
                referenceSex: $growth.referenceSex,
                offersNoReference: !growth.hasExistingReference,
                hasDueDate: $growth.hasDueDate,
                dueDateNeedsReference: true,
                dueDate: $growth.dueDate,
                growthFooter: BabyEditCopy.referenceFooter,
                showsGrowth: growth.loaded,
                identifierPrefix: "baby"
            ) {
                if growth.failed {
                    InlineError(BabyEditCopy.profileLoadFailed)
                        .accessibilityIdentifier("baby.growthLoadError")
                    Button(action: loadGrowthProfile) {
                        Text(BabyEditCopy.retryGrowthSettings)
                            .fixedSize(horizontal: false, vertical: true)
                            .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                            .contentShape(Rectangle())
                    }
                    .accessibilityIdentifier("baby.retryGrowthProfile")
                } else {
                    ProgressView()
                }
            }

            // Deleting a baby removes it for the whole family, including
            // every record every caregiver logged against it, so only the
            // owner gets the control at all. Hidden rather than disabled:
            // a greyed-out delete invites a tap and explains nothing.
            CardSection {
                if canDeleteBaby {
                    DestructiveButton(BabyEditCopy.deleteLabel, identifier: "baby.delete") {
                        isConfirmingDelete = true
                    }
                } else {
                    Text(BabyEditCopy.deleteOwnerOnly)
                        .font(.footnote)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("baby.deleteOwnerOnly")
                }
            } footer: {
                Text(canDeleteBaby ? BabyEditCopy.sharedDeleteFooter : BabyEditCopy.deleteFooter)
            }
        }
        .tint(Palette.feed)
        .task { if !growth.loaded { loadGrowthProfile() } }
        .confirmDelete(
            isPresented: $isConfirmingDelete,
            title: BabyEditCopy.deleteConfirmTitle(name: baby.name),
            deleteTitle: BabyEditCopy.deleteConfirmAction,
            keepTitle: BabyEditCopy.deleteKeepAction,
            identifierPrefix: "baby"
        ) {
            do {
                try store.deleteBaby(baby)
                errorReporter.clear()
                onDeleted(baby.id)
                onDone()
            } catch {
                // A failed delete that dismissed looked exactly like a
                // successful one until the baby reappeared.
                errorReporter.report(
                    ErrorBannerCopy.deleteFailed(what: ErrorSubject.baby),
                    error: error,
                    category: AppLog.store
                )
            }
        }
    }

    private func save() {
        do {
            try BabyGrowthEditValidation.save(
                store: store, baby: baby, name: name, birthDate: birthDate,
                // A failed/uncompleted profile read explicitly preserves profile state.
                referenceSex: growth.referenceForSave,
                expectedDueDate: try growth.dueDateForSave()
            ) {
                errorReporter.clear()
                onDone()
            }
        } catch {
            // Stays open: dismissing would throw away the name
            // the user just typed and look like it saved.
            errorReporter.report(
                ErrorBannerCopy.saveFailed(what: ErrorSubject.baby),
                error: error,
                category: AppLog.store
            )
        }
    }
}

enum BabyGrowthEditValidation {
    @MainActor static func save(store: BabyStore, baby: Baby, name: String, birthDate: Date,
                                referenceSex: GrowthReferenceSex?, expectedDueDate: LocalDate?,
                                onDone: () -> Void) throws {
        try store.updateBabyAndGrowthProfile(baby, name: name, birthDate: birthDate,
                                             referenceSex: referenceSex, expectedDueDate: expectedDueDate)
        onDone()
    }

    static func pickerDate(_ day: LocalDate?, timeZone: TimeZone = .current) -> Date? {
        guard let day else { return nil }
        let parts = day.iso8601.split(separator: "-").compactMap { Int($0) }
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = timeZone
        return calendar.date(from: DateComponents(year: parts[0], month: parts[1], day: parts[2], hour: 12))
    }
}

/// Read failure is distinct from a successful fetch returning no legacy profile.
/// The save boundary deliberately preserves profile state until a read succeeds.
struct BabyGrowthProfileEditor {
    private(set) var loaded = false
    private(set) var failed = false
    private(set) var hasExistingReference = false
    var referenceSex: GrowthReferenceSex?
    var hasDueDate = false
    var dueDate: Date

    init(fallbackDate: Date) { dueDate = fallbackDate }

    @MainActor mutating func load(fetch: () throws -> GrowthProfile?, onFailure: (any Error) -> Void) {
        do {
            let profile = try fetch()
            referenceSex = profile?.referenceSex
            hasExistingReference = referenceSex != nil
            hasDueDate = profile?.expectedDueDate != nil
            if let date = BabyGrowthEditValidation.pickerDate(profile?.expectedDueDate) { dueDate = date }
            loaded = true
            failed = false
        } catch {
            loaded = false
            failed = true
            onFailure(error)
        }
    }

    var referenceForSave: GrowthReferenceSex? { loaded ? referenceSex : nil }

    func dueDateForSave() throws -> LocalDate? {
        guard loaded, referenceSex != nil, hasDueDate else { return nil }
        return try LocalDate(displaying: dueDate, in: .current)
    }
}
