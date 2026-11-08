import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public struct AddBabySheet: View {
    let store: BabyStore
    let onAdded: (Baby) -> Void
    let onCancel: (() -> Void)?
    /// First launch only, and only where sharing exists. A caregiver joining
    /// a family has no baby of their own to invent: the family's babies
    /// arrive with the join, so this is their way past the screen.
    let onJoinFamily: (() -> Void)?

    @State private var name = ""
    @State private var birthDate = Date()
    @State private var referenceSex: GrowthReferenceSex?
    @State private var hasDueDate = false
    @State private var dueDate = Date()
    @Environment(\.errorReporter) private var errorReporter

    public init(
        store: BabyStore,
        onAdded: @escaping (Baby) -> Void,
        onCancel: (() -> Void)? = nil,
        onJoinFamily: (() -> Void)? = nil
    ) {
        self.store = store
        self.onAdded = onAdded
        self.onCancel = onCancel
        self.onJoinFamily = onJoinFamily
    }

    private var canSave: Bool {
        AddBabyValidation.canSave(name: name, referenceSex: referenceSex)
    }

    /// Presented full height (the switcher's sheet), or as the whole screen
    /// on first launch, where there is no close button.
    public var body: some View {
        SheetScaffold(
            L10n.tr("Add baby"),
            closeTitle: L10n.tr("Cancel"),
            onClose: onCancel,
            primary: SheetAction(L10n.tr("Save"), identifier: "addBaby.save", isEnabled: canSave, action: save)
        ) {
            BabyFormFields(
                name: $name,
                birthDate: $birthDate,
                referenceSex: $referenceSex,
                hasDueDate: $hasDueDate,
                dueDate: $dueDate,
                growthFooter: L10n.tr("Choose the reference used for growth comparisons. The expected due date is optional."),
                identifierPrefix: "addBaby"
            )

            // Below the fields and plainly secondary: adding a baby is
            // what nearly everyone opening the app for the first time
            // is here to do.
            if let onJoinFamily {
                CardSection {
                    Button(action: onJoinFamily) {
                        Text(AddBabyCopy.joinFamily)
                            .fixedSize(horizontal: false, vertical: true)
                            .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                            .contentShape(Rectangle())
                    }
                    .accessibilityIdentifier("addBaby.joinFamily")
                } footer: {
                    Text(AddBabyCopy.joinFamilyFooter)
                }
            }
        }
        .tint(Palette.feed)
    }

    private func save() {
        guard let referenceSex else { return }
        do {
            try AddBabyValidation.save(
                store: store,
                name: name.trimmingCharacters(in: .whitespaces),
                birthDate: birthDate,
                referenceSex: referenceSex,
                expectedDueDate: try AddBabyValidation.dueDate(enabled: hasDueDate, date: dueDate)
            ) { baby in
                errorReporter.clear()
                onAdded(baby)
            }
        } catch {
            // On first launch this sheet IS the app: a silent
            // return here left a parent tapping Save forever.
            errorReporter.report(
                ErrorBannerCopy.saveFailed(what: ErrorSubject.baby),
                error: error,
                category: AppLog.store
            )
        }
    }
}

enum AddBabyCopy {
    static let joinFamily = L10n.tr("Join a family")
    static let joinFamilyFooter = L10n.tr("Already tracking with another caregiver? Join their family to see the same babies and records.")
}

/// Date picker values are converted at the boundary; stored due dates never contain a time.
enum AddBabyValidation {
    @MainActor static func save(store: BabyStore, name: String, birthDate: Date,
                                referenceSex: GrowthReferenceSex, expectedDueDate: LocalDate?,
                                onAdded: (Baby) -> Void) throws {
        let baby = try store.addBaby(name: name, birthDate: birthDate,
                                     referenceSex: referenceSex, expectedDueDate: expectedDueDate)
        onAdded(baby)
    }

    static func canSave(name: String, referenceSex: GrowthReferenceSex?) -> Bool {
        !name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && referenceSex != nil
    }

    static func dueDate(enabled: Bool, date: Date, timeZone: TimeZone = .current) throws -> LocalDate? {
        enabled ? try LocalDate(displaying: date, in: timeZone) : nil
    }
}
