import BabyCore
import BabyStore
import BabySync
import BabyUI
import SwiftUI

/// Asks for the name other caregivers will see.
///
/// Shown whenever Apple did not give us one — which is every authorization
/// after the first, and every additional device. The name is never derived
/// from an email address or Apple's subject claim: those are identifiers, not
/// something a person chose to be called, and putting either in front of the
/// rest of a family would leak it.
public struct DisplayNameConfirmationView<Model: FamilySyncSettingsModel>: View {
    @Bindable private var model: Model
    @State private var name: String
    @State private var isSaving = false
    private let onConfirmed: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    public init(model: Model, onConfirmed: @escaping () -> Void = {}) {
        self.model = model
        self.onConfirmed = onConfirmed
        _name = State(initialValue: model.profile.displayName)
    }

    public var body: some View {
        Form {
            Section {
                TextField(DisplayNameCopy.field, text: $name)
                    .textContentType(.name)
                    .frame(minHeight: 44)
                    .accessibilityIdentifier("displayName.field")
            } header: {
                Text(DisplayNameCopy.header)
            } footer: {
                Text(DisplayNameCopy.footer)
            }

            Section {
                Button(DisplayNameCopy.confirm) {
                    isSaving = true
                    Task {
                        defer { isSaving = false }
                        do {
                            try await model.confirmDisplayName(name)
                            onConfirmed()
                        } catch {
                            errorReporter.report(DisplayNameCopy.failed, error: error, category: AppLog.store)
                        }
                    }
                }
                .frame(minHeight: 44)
                .disabled(name.trimmingCharacters(in: .whitespaces).isEmpty || isSaving)
                .accessibilityIdentifier("displayName.confirm")
            }
        }
        .navigationTitle(DisplayNameCopy.title)
    }

}

enum DisplayNameCopy {
    static let title = String(localized: "Your name", bundle: .module)
    static let header = String(localized: "Display name", bundle: .module)
    static let field = String(localized: "Name", bundle: .module)
    static let footer = String(localized: "Other caregivers in your family see this name on the records you log.", bundle: .module)
    static let confirm = String(localized: "Confirm", bundle: .module)
    static let failed = String(localized: "That name couldn't be saved.", bundle: .module)
}
