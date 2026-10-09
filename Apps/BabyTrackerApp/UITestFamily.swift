import Foundation
import BabyCore
import BabyStore
import BabySync
import SettingsFeature

/// A signed-in owner in a family with members, for XCUITest — no server.
///
/// Behind `#if DEBUG` like the rest of `UITestSupport`, and behind the
/// `-uiTestOwnerFamily` launch argument. `-uiTestNoFamily` is the same
/// caregiver signed in with no family yet, whose server previews every
/// invitation as `invitedFamilyName` and lets it be redeemed once. The model is the production
/// `FamilySettingsViewModel`, signed in through the production
/// `AccountCoordinator`; only the far side of each seam is scripted: the
/// identity server, Apple, the Keychain (in memory), and the family server,
/// which keeps its members and invitations in memory and forgets them when the
/// app quits. No engine is built, so nothing syncs and the store stays in the
/// local realm the rest of the suite expects.
#if DEBUG
@MainActor
enum UITestFamily {
    static let argument = "-uiTestOwnerFamily"
    static var isRequested: Bool { ProcessInfo.processInfo.arguments.contains(argument) }
    static let noFamilyArgument = "-uiTestNoFamily"
    static var isNoFamilyRequested: Bool { ProcessInfo.processInfo.arguments.contains(noFamilyArgument) }
    static let homeSyncNeedsSignInArgument = "-uiTestHomeSyncNeedsSignIn"
    static var isHomeSyncNeedsSignInRequested: Bool {
        ProcessInfo.processInfo.arguments.contains(homeSyncNeedsSignInArgument)
    }
    nonisolated static let invitedFamilyName = "The Kovals"
    /// Named by an invitation whose token starts with "Second", so a test can
    /// tell two links apart.
    nonisolated static let secondInvitedFamilyName = "The Petrenkos"

    // Fixed so a UI test can name each row by its identifier.
    nonisolated static let ownerID = UUID(uuidString: "0A000000-0000-4000-8000-000000000001")!
    nonisolated static let caregiverID = UUID(uuidString: "0A000000-0000-4000-8000-000000000002")!
    nonisolated static let viewerID = UUID(uuidString: "0A000000-0000-4000-8000-000000000003")!
    /// Listed by the server with no code on this phone: minted on another
    /// phone, or before a reinstall.
    nonisolated static let otherPhoneInvitationID = UUID(uuidString: "0A000000-0000-4000-8000-0000000000F1")!

    private nonisolated static let origin = try! ServerOrigin(validating: "https://sync.example.test")
    private nonisolated static let instanceID = UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    private nonisolated static let familyID = UUID(uuidString: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    private nonisolated static let deviceID = UUID(uuidString: "f1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
    private static let defaultsSuite = "com.olhapi.babytracker.uiTestOwnerFamily"

    static func makeOwnerFamily(store: BabyStore) -> FamilySettingsViewModel {
        make(store: store, inFamily: true)
    }

    static func makeNoFamily(store: BabyStore) -> FamilySettingsViewModel {
        make(store: store, inFamily: false)
    }

    static func makeHomeSyncNeedsSignIn(store: BabyStore) -> FamilySettingsViewModel {
        UserDefaults().removePersistentDomain(forName: defaultsSuite)
        let defaults = UserDefaults(suiteName: defaultsSuite)!
        let backend = InMemoryKeychainBackend()
        let endpoint = ConfirmedEndpoint(origin: origin, serverInstanceID: instanceID)
        let coordinator = AccountCoordinator(
            store: store,
            keychain: KeychainSessionStore(backend: backend),
            identity: Identity(info: serverInfo, bundle: sessionBundle(inFamily: true)),
            apple: Apple(), credentialState: CredentialState(), defaults: defaults
        )
        let engines = RecoveryEngineSource()
        let model = FamilySettingsViewModel(
            store: store, coordinator: coordinator, client: Server(inFamily: true), defaults: defaults,
            makeEngine: { _, _ in engines.make() },
            codeStoreBackend: backend, startLiveActivity: { _, _ in }
        )
        Task {
            do {
                try await model.confirmDisplayName("Olena")
                try await model.confirmEndpoint(endpoint)
                try await model.signInWithApple()
                let realm = RealmID.shared(
                    canonicalOrigin: origin.canonicalString, serverInstanceID: instanceID, familyID: familyID
                )
                let checkpoint = UUID()
                try store.beginTransition(to: realm, checkpointID: checkpoint)
                try store.commitTransition(checkpointID: checkpoint)
                let baby = try store.addBaby(name: "Test Baby", birthDate: .now, referenceSex: .girls, expectedDueDate: nil)
                _ = try store.logBottle(volumeML: 90, contents: .formula, for: baby.id)
            } catch {
                AppLog.store.error("home sync UI fixture failed: \(String(reflecting: type(of: error)), privacy: .public)")
            }
        }
        return model
    }

    @MainActor private final class RecoveryEngineSource {
        var buildCount = 0
        func make() -> any FamilySyncDriving {
            buildCount += 1
            return RecoveryEngine(status: buildCount == 1 ? .needsSignIn : .upToDate(.now))
        }
    }

    @MainActor private final class RecoveryEngine: FamilySyncDriving {
        let status: FamilySyncEngine.Status
        init(status: FamilySyncEngine.Status) { self.status = status }
        func activateAsOwner() async throws {}
        func activateAsJoiner() async throws {}
        func syncNow(_ reason: SyncReason) async {}
        func localWriteHappened() {}
        func startForegroundPolling() {}
        func stopForegroundPolling() {}
        func stop() async {}
    }

    private static func make(store: BabyStore, inFamily: Bool) -> FamilySettingsViewModel {
        // Its own suite, emptied every launch, so nothing here reaches the
        // App Group records a real session would use.
        UserDefaults().removePersistentDomain(forName: defaultsSuite)
        let defaults = UserDefaults(suiteName: defaultsSuite)!
        let backend = InMemoryKeychainBackend()
        let endpoint = ConfirmedEndpoint(origin: origin, serverInstanceID: instanceID)

        let coordinator = AccountCoordinator(
            store: store,
            keychain: KeychainSessionStore(backend: backend),
            identity: Identity(info: serverInfo, bundle: sessionBundle(inFamily: inFamily)),
            apple: Apple(),
            credentialState: CredentialState(),
            defaults: defaults
        )
        let model = FamilySettingsViewModel(
            store: store, coordinator: coordinator, client: Server(inFamily: inFamily), defaults: defaults,
            codeStoreBackend: backend, startLiveActivity: { _, _ in }
        )
        Task {
            do {
                try await model.confirmDisplayName("Olena")
                try await model.confirmEndpoint(endpoint)
                try await model.signInWithApple()
            } catch {
                AppLog.store.error("family UI fixture failed: \(error.localizedDescription, privacy: .public)")
            }
        }
        return model
    }

    private static var serverInfo: ServerInfo {
        ServerInfo(
            serverInstanceID: instanceID, canonicalOrigin: origin.canonicalString, recoveryEpoch: 0,
            capabilities: ["records-v1"], appleAuthorizationMode: "hosted",
            maxItemsPerPage: 200, maxEncodedBytes: 1_000_000,
            sourceVersion: "ui-test", sourceURL: "https://example.test"
        )
    }

    private static func sessionBundle(inFamily: Bool) -> SessionBundle {
        SessionBundle(
            accountID: ownerID, deviceID: deviceID, recoveryEpoch: 0,
            accessToken: "ui-test-access", refreshToken: "ui-test-refresh",
            expiresAt: .distantFuture, deviceCredential: "ui-test-device", familyID: inFamily ? familyID : nil
        )
    }

    // MARK: The far side of each seam

    private struct Identity: IdentityClient {
        let info: ServerInfo
        let bundle: SessionBundle
        func serverInfo(_ origin: ServerOrigin) async throws -> ServerInfo { info }
        func challenge(_ endpoint: ConfirmedEndpoint) async throws -> String {
            "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
        }
        func exchangeApple(_ request: AppleExchangeRequest, at endpoint: ConfirmedEndpoint) async throws -> SessionBundle {
            bundle
        }
    }

    private struct Apple: AppleSignInClient {
        func requestProof(nonce: String) async throws -> AppleProof {
            AppleProof(identityToken: "ui-test", authorizationCode: "ui-test", userIdentifier: "ui-test", fullName: "Olena")
        }
    }

    private struct CredentialState: AppleCredentialStateClient {
        func credentialState(forUserIdentifier userIdentifier: String) async throws -> AppleCredentialState {
            .authorized
        }
    }

    /// The family server: three members and one invitation minted elsewhere,
    /// held in memory — or, for `-uiTestNoFamily`, no family until an
    /// invitation is redeemed. Mints, lists newest first, and revokes the way the real
    /// one does, including the link shape (`/invite#token=…`).
    private actor Server: FamilyClient {
        private var members: [FamilyMember] = [
            FamilyMember(id: UITestFamily.ownerID, displayName: "Olena", role: "owner"),
            FamilyMember(id: UITestFamily.caregiverID, displayName: "Marta", role: "caregiver"),
            FamilyMember(id: UITestFamily.viewerID, displayName: "Grandma", role: "viewer"),
        ]
        private var ownerRole: String
        init(inFamily: Bool) { ownerRole = inFamily ? "owner" : "caregiver" }
        private var pending: [PendingInvitation] = [
            PendingInvitation(
                id: UITestFamily.otherPhoneInvitationID, role: .viewer,
                // Six calendar days, not 6 days and an hour: after 23:00 the
                // extra hour crossed midnight and the row read "Expires in 7
                // days", which the audit test does not expect.
                expiresAt: Calendar.current.date(byAdding: .day, value: 6, to: .now)!,
                createdAt: .now.addingTimeInterval(-86_400)
            ),
        ]

        func createFamily(name: String, displayName: String) async throws -> FamilyMembership { membership }
        func confirmJoin(token: String, displayName: String) async throws -> FamilyMembership { membership }
        func members() async throws -> [FamilyMember] { members }
        func profile() async throws -> CaregiverProfile {
            CaregiverProfile(accountID: UITestFamily.ownerID, displayName: "Olena", role: ownerRole)
        }
        func updateDisplayName(_ name: String) async throws -> CaregiverProfile { try await profile() }

        func createInvitation(role: FamilyRole) async throws -> FamilyInvitation {
            let token = (0..<43).map { _ in "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_".randomElement()! }
            let invitation = FamilyInvitation(
                id: UUID(),
                shareURL: URL(string: "https://sync.example.test/invite#token=\(String(token))")!,
                role: role,
                expiresAt: .now.addingTimeInterval(7 * 86_400)
            )
            pending.append(PendingInvitation(
                id: invitation.id, role: role, expiresAt: invitation.expiresAt, createdAt: .now
            ))
            return invitation
        }
        func pendingInvitations() async throws -> [PendingInvitation] {
            pending.sorted { $0.createdAt > $1.createdAt }
        }
        func revokeInvitation(_ invitationID: UUID) async throws {
            pending.removeAll { $0.id == invitationID }
        }
        func previewInvitation(token: String) async throws -> InvitationPreview {
            InvitationPreview(
                familyName: token.hasPrefix("Second") ? UITestFamily.secondInvitedFamilyName : UITestFamily.invitedFamilyName,
                expiresAt: .now.addingTimeInterval(86_400)
            )
        }

        func perform(_ command: FamilyLifecycleCommand) async throws {
            switch command {
            case .removeMember(let id):
                members.removeAll { $0.id == id }
            case .changeRole(let id, let role):
                members = members.map { $0.id == id ? FamilyMember(id: id, displayName: $0.displayName, role: role.rawValue) : $0 }
            case .transferOwnership(let id):
                members = members.map {
                    FamilyMember(id: $0.id, displayName: $0.displayName, role: $0.id == id ? "owner" : ($0.role == "owner" ? "caregiver" : $0.role))
                }
                ownerRole = "caregiver"
            case .deleteAccount:
                break
            }
        }

        func leaveFamily() async throws {}
        func scheduleFamilyDeletion() async throws {}
        func recoverFamily() async throws -> [FamilyMember] { members }
        func forfeitFamilyRecovery() async throws {}
        func logout() async throws {}

        private var membership: FamilyMembership {
            FamilyMembership(familyID: UITestFamily.familyID, accountID: UITestFamily.ownerID, deviceID: UITestFamily.deviceID, recoveryEpoch: 0)
        }
    }
}
#endif
