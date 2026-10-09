import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// Where an invitation that arrives from outside the Caregivers screen goes:
/// a `babytracker://` link opened by the system, or a QR code the in-app
/// scanner reads. Neither ever redeems anything by itself.
@Suite("Invitation routing")
struct InvitationRoutingTests {
    private static let token = "Abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"
    private static let origin = try! ServerOrigin(validating: "https://sync.example.test")
    private static let link = URL(string: "babytracker://invite?server=https%3A%2F%2Fsync.example.test&token=\(token)")!

    @Test func deepLinkWhileInAFamilyOnlyAlerts() {
        #expect(InvitationRouting.route(url: Self.link, hasFamily: true) == .alreadyInFamily)
    }

    @Test func deepLinkWithoutFamilyOpensJoinWithTheIntent() {
        let route = InvitationRouting.route(url: Self.link, hasFamily: false)
        #expect(route == .join(InvitationIntent(origin: Self.origin, token: Self.token)))
    }

    private static let familyKey = FamilySessionKey(
        canonicalOrigin: origin.canonicalString, serverInstanceID: UUID(), familyID: UUID(),
        accountID: UUID(), deviceID: UUID(), recoveryEpoch: 0
    )

    /// A family scheduled for deletion is still this phone's family: the link
    /// only alerts, and never opens the Join sheet over it.
    @Test func deepLinkWhileTheFamilyIsInRecoveryOnlyAlerts() {
        #expect(InvitationRouting.route(url: Self.link, mode: .recovery(Self.familyKey)) == .alreadyInFamily)
        #expect(InvitationRouting.route(url: Self.link, mode: .shared(Self.familyKey)) == .alreadyInFamily)
    }

    /// Signed out keeps no family session, so it goes to the Join sheet,
    /// which asks to sign in again first.
    @Test func deepLinkWhileSignedOutOrWithoutFamilyOpensJoin() {
        let intent = InvitationIntent(origin: Self.origin, token: Self.token)
        let accountKey = Self.familyKey.accountSessionKey
        #expect(InvitationRouting.route(url: Self.link, mode: .signedOut(accountKey)) == .join(intent))
        #expect(InvitationRouting.route(url: Self.link, mode: .authenticated(accountKey)) == .join(intent))
        #expect(InvitationRouting.route(url: Self.link, mode: .localOnly) == .join(intent))
    }

    @Test func nonInvitationURLIsIgnored() {
        #expect(InvitationRouting.route(url: URL(string: "babytracker://feed")!, hasFamily: false) == .ignore)
        #expect(InvitationRouting.route(url: URL(string: "babytracker://feed")!, hasFamily: true) == .ignore)
        #expect(InvitationRouting.route(url: URL(string: "https://example.test/elsewhere")!, hasFamily: false) == .ignore)
    }

    /// The scanner's payload handling, without a camera: text that is not an
    /// invitation is refused and scanning carries on; the first payload that
    /// is one wins.
    @Test func scannerIgnoresNonInvitationPayloads() {
        let scan = { (payloads: [String]) in
            InvitationScanning.outcome(for: payloads, confirmedOrigin: Self.origin)
        }

        #expect(scan([]) == .nothing)
        #expect(scan(["https://example.test/menu"]) == .notAnInvitation)
        #expect(scan(["hello"]) == .notAnInvitation)
        #expect(
            scan(["https://example.test/menu", Self.link.absoluteString])
                == .invitation(InvitationIntent(origin: Self.origin, token: Self.token))
        )
        // A bare code is paired with the server this phone already confirmed.
        #expect(scan([Self.token]) == .invitation(InvitationIntent(origin: Self.origin, token: Self.token)))
    }
}

/// The hand-off `RootView` waits on: busy for exactly as long as any
/// Caregivers screen still has a presentation up.
@MainActor
@Suite("Invitation link hand-off")
struct InvitationLinkHandOffTests {
    @Test func busyUntilEveryPresentingScreenHasReleased() {
        let handOff = InvitationLinkHandOff()
        let settingsTab = UUID()
        let joinSheet = UUID()
        #expect(!handOff.isBusy)

        handOff.presenting(settingsTab)
        handOff.presenting(joinSheet)
        handOff.linkArrived()
        #expect(handOff.arrivals == 1)
        #expect(handOff.isBusy)

        handOff.released(settingsTab)
        #expect(handOff.isBusy, "free while the Join sheet's confirmation is still going")
        handOff.released(joinSheet)
        #expect(!handOff.isBusy)
    }
}

/// What "Scan QR code" does, decided before any camera is touched.
/// `DataScannerViewController.isAvailable` is false until access is granted,
/// so an undecided permission must be asked for rather than read as "no
/// camera".
@Suite("Scanner access")
struct ScannerAccessTests {
    @Test func undecidedPermissionIsAskedForFirst() {
        #expect(ScannerAccess.decide(authorization: .notDetermined, isSupported: true, isAvailable: false) == .requestAccess)
        #expect(ScannerAccess.decide(authorization: .notDetermined, isSupported: true, isAvailable: true) == .requestAccess)
    }

    @Test func grantedAndAvailableScans() {
        #expect(ScannerAccess.decide(authorization: .authorized, isSupported: true, isAvailable: true) == .scan)
    }

    @Test func grantedButUnavailableSaysSoWithoutSettings() {
        #expect(
            ScannerAccess.decide(authorization: .authorized, isSupported: true, isAvailable: false)
                == .unavailable(showSettings: false)
        )
    }

    @Test func refusedOrRestrictedOffersSettings() {
        for status in [CameraAuthorization.denied, .restricted] {
            #expect(
                ScannerAccess.decide(authorization: status, isSupported: true, isAvailable: false)
                    == .unavailable(showSettings: true)
            )
        }
    }

    /// No scanner on this device at all: nothing to ask for, and nothing
    /// Settings could change.
    @Test func unsupportedDeviceNeverAsksOrOffersSettings() {
        for status in [CameraAuthorization.notDetermined, .authorized, .denied, .restricted] {
            #expect(
                ScannerAccess.decide(authorization: status, isSupported: false, isAvailable: true)
                    == .unavailable(showSettings: false)
            )
        }
    }
}

/// How long an invitation waits for "Join {family}?" on the Caregivers
/// screen: through choosing a server and signing in, but not past the phone
/// leaving `.authenticated` — a finished join, a created family, a sign-out.
@Suite("Pending invitation lifetime")
struct PendingInvitationLifetimeTests {
    private static let origin = try! ServerOrigin(validating: "https://sync.example.test")
    private static let familyKey = FamilySessionKey(
        canonicalOrigin: origin.canonicalString, serverInstanceID: UUID(), familyID: UUID(),
        accountID: UUID(), deviceID: UUID(), recoveryEpoch: 0
    )
    private static var accountKey: AccountSessionKey { familyKey.accountSessionKey }
    private static var endpoint: ConfirmedEndpoint {
        ConfirmedEndpoint(origin: origin, serverInstanceID: familyKey.serverInstanceID)
    }

    @Test func waitsThroughServerChoiceAndSignIn() {
        #expect(JoinConfirmationRequest.invitationSurvives(from: .localOnly, to: .endpointConfirmed(Self.endpoint)))
        #expect(JoinConfirmationRequest.invitationSurvives(
            from: .endpointConfirmed(Self.endpoint), to: .authenticated(Self.accountKey)
        ))
        #expect(JoinConfirmationRequest.invitationSurvives(
            from: .signedOut(Self.accountKey), to: .authenticated(Self.accountKey)
        ))
    }

    /// A finished join moves the phone to `.shared` before "Join {family}?"
    /// can clear the invitation; kept, it would reappear after the next
    /// sign-in for an invitation already used.
    @Test func isSpentOnceThePhoneLeavesAuthenticated() {
        #expect(!JoinConfirmationRequest.invitationSurvives(from: .authenticated(Self.accountKey), to: .shared(Self.familyKey)))
        #expect(!JoinConfirmationRequest.invitationSurvives(
            from: .authenticated(Self.accountKey), to: .signedOut(Self.accountKey)
        ))
        #expect(!JoinConfirmationRequest.invitationSurvives(from: .authenticated(Self.accountKey), to: .localOnly))
    }
}
