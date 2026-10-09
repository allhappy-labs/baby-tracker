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

    public var body: some View {
        NavigationStack {
            Form {
                TextField(L10n.tr("Name"), text: $name)
                    #if os(iOS)
                    // Autocapitalization is a UIKit text-input concept; no macOS equivalent.
                    .textInputAutocapitalization(.words)
                    #endif
                DatePicker(L10n.tr("Born"), selection: $birthDate, displayedComponents: [.date])

                Picker(L10n.tr("Swiss PEZZ 2025 reference"), selection: $referenceSex) {
                    Text(L10n.tr("Choose a reference")).tag(GrowthReferenceSex?.none)
                    Text(L10n.tr("Girls")).tag(GrowthReferenceSex?.some(.girls))
                    Text(L10n.tr("Boys")).tag(GrowthReferenceSex?.some(.boys))
                }
                .accessibilityIdentifier("addBaby.referenceSex")

                Toggle(L10n.tr("Add expected due date"), isOn: $hasDueDate)
                    .accessibilityIdentifier("addBaby.hasDueDate")
                if hasDueDate {
                    DatePicker(L10n.tr("Expected due date"), selection: $dueDate, displayedComponents: [.date])
                        .accessibilityIdentifier("addBaby.dueDate")
                }
                Text(L10n.tr("Choose the reference used for growth comparisons. The expected due date is optional."))
                    .font(.footnote)

                // Below the fields and plainly secondary: adding a baby is
                // what nearly everyone opening the app for the first time
                // is here to do.
                if let onJoinFamily {
                    Section {
                        Button(AddBabyCopy.joinFamily, action: onJoinFamily)
                            .frame(minHeight: 44)
                            .accessibilityIdentifier("addBaby.joinFamily")
                    } footer: {
                        Text(AddBabyCopy.joinFamilyFooter)
                    }
                }
            }
            .navigationTitle(L10n.tr("Add baby"))
            #if os(iOS)
            // Inline title display mode is a UIKit navigation-bar concept.
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                if let onCancel {
                    ToolbarItem(placement: .cancellationAction) {
                        Button(L10n.tr("Cancel"), action: onCancel)
                    }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button(L10n.tr("Save")) {
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
                    .disabled(!canSave)
                    .accessibilityIdentifier("addBaby.save")
                }
            }
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
