import BabyCore
import BabyStore
import BabySync
import BabyUI
import SwiftUI

/// The account, as distinct from the family: who you are, and how to leave
/// entirely.
///
/// Account deletion is separated from every family action because it is the
/// one that ends access on every device, and because it is blocked while you
/// still own a family — a rule a user only discovers if the screen says so
/// before they tap.
public struct AccountSettingsView<Model: FamilySyncSettingsModel>: View {
    @Bindable private var model: Model
    @State private var editedName = ""
    @State private var showingDeletion = false
    @State private var isWorking = false
    @Environment(\.errorReporter) private var errorReporter

    public init(model: Model) {
        self.model = model
        _editedName = State(initialValue: model.profile.displayName)
    }

    public var body: some View {
        Form {
            Section {
                TextField(AccountCopy.nameField, text: $editedName)
                    .textContentType(.name)
                    .frame(minHeight: 44)
                    .accessibilityIdentifier("account.nameField")

                Button(AccountCopy.save) {
                    perform { try await model.updateDisplayName(editedName) }
                }
                .frame(minHeight: 44)
                .disabled(editedName.trimmingCharacters(in: .whitespaces).isEmpty || isWorking)
                .accessibilityIdentifier("account.saveName")
            } header: {
                Text(AccountCopy.profileHeader)
            } footer: {
                Text(AccountCopy.profileFooter)
            }

            Section {
                LabeledContent(AccountCopy.roleLabel, value: model.isOwner ? AccountCopy.owner : AccountCopy.caregiver)
                    .accessibilityIdentifier("account.role")
            }

            if model.isOwner && model.hasFamily {
                Section {
                    if let client = model.makeExportClient() {
                        NavigationLink(AccountCopy.exportFamilyData) {
                            FamilyExportView(client: client, persistenceKey: exportPersistenceKey)
                        }
                            .frame(minHeight: 44)
                            .accessibilityIdentifier("account.export")
                    } else {
                        LabeledContent(AccountCopy.exportFamilyData, value: model.exportAvailable == nil ? AccountCopy.checking : AccountCopy.unavailable)
                            .accessibilityIdentifier("account.exportUnavailable")
                    }
                } footer: {
                    Text(model.exportAvailable == false
                         ? AccountCopy.exportUnavailableFooter
                         : AccountCopy.exportFooter)
                }
            }

            Section {
                Button(AccountCopy.deleteAccount, role: .destructive) { showingDeletion = true }
                    .frame(minHeight: 44)
                    .accessibilityIdentifier("account.delete")
            } footer: {
                Text(model.isOwner && model.hasFamily ? AccountCopy.ownerBlockedFooter : AccountCopy.deleteFooter)
            }
        }
        .navigationTitle(AccountCopy.title)
        .task { await model.refreshExportAvailability() }
        .disabled(isWorking)
        .fittedSheet(isPresented: $showingDeletion) {
            FamilyLifecycleConfirmationView(
                confirmation: .init(
                    title: AccountCopy.deleteTitle, body: FamilyLifecycleCopy.deleteAccount,
                    confirmLabel: AccountCopy.deleteConfirm
                ) {
                    try await model.deleteAccount()
                }
            ) {
                showingDeletion = false
                perform { try await model.deleteAccount() }
            } onCancel: {
                showingDeletion = false
            }
        }
    }

    private var exportPersistenceKey: String? {
        let session: FamilySessionKey
        switch model.mode {
        case .shared(let key), .recovery(let key): session = key
        default: return nil
        }
        return "baby-tracker-export-v1.\(session.serverInstanceID.uuidString.lowercased()).\(session.recoveryEpoch).\(session.familyID.uuidString.lowercased()).\(session.accountID.uuidString.lowercased())"
    }

    private func perform(_ work: @escaping () async throws -> Void) {
        isWorking = true
        Task {
            defer { isWorking = false }
            do {
                try await work()
                errorReporter.clear()
            } catch FamilySyncError.ownerTransferRequired {
                errorReporter.report(AccountCopy.transferFirst, error: FamilySyncError.ownerTransferRequired, category: AppLog.store)
            } catch {
                errorReporter.report(AccountCopy.failed, error: error, category: AppLog.store)
            }
        }
    }

}

enum AccountCopy {
    static let title = String(localized: "Account", bundle: .module)
    static let profileHeader = String(localized: "Your profile", bundle: .module)
    static let profileFooter = String(localized: "Other caregivers see this name on records you log.", bundle: .module)
    static let nameField = String(localized: "Name", bundle: .module)
    static let save = String(localized: "Save name", bundle: .module)
    static let roleLabel = String(localized: "Role", bundle: .module)
    static let owner = String(localized: "Owner", bundle: .module)
    static let caregiver = String(localized: "Caregiver", bundle: .module)
    static let exportFamilyData = String(localized: "Export family data", bundle: .module)
    static let checking = String(localized: "Checking…", bundle: .module)
    static let unavailable = String(localized: "Unavailable", bundle: .module)
    static let exportUnavailableFooter = String(localized: "This server does not currently offer family data export.", bundle: .module)
    static let exportFooter = String(localized: "Download a JSON copy of this family's records.", bundle: .module)
    static let deleteAccount = String(localized: "Delete account", bundle: .module)
    static let deleteTitle = String(localized: "Delete your account?", bundle: .module)
    static let deleteConfirm = String(localized: "Delete account", bundle: .module)
    static let deleteFooter = String(localized: "This removes your caregiver profile and this device's access.", bundle: .module)
    static let ownerBlockedFooter = String(localized: "You own a family. Hand ownership to another caregiver before deleting your account.", bundle: .module)
    static let transferFirst = String(localized: "Hand ownership to another caregiver first.", bundle: .module)
    static let failed = String(localized: "That didn't work. Nothing was changed.", bundle: .module)
}
