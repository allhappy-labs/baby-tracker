import BabyCore
import BabyStore
import BabySync
import BabyUI
import SwiftUI

/// The Caregivers screen: who is in this family, how to invite someone, and
/// every way out of it.
///
/// It is deliberately one screen rather than a hub of six. A parent opens this
/// to answer one of three questions — who can see my data, how do I add my
/// partner, how do I stop — and each of those should be visible without
/// navigating first.
public struct FamilySettingsView<Model: FamilySyncSettingsModel>: View {
    @Bindable private var model: Model
    @Environment(\.errorReporter) private var errorReporter

    @State private var familyName = ""
    /// The invite sheet, on its first step or showing a code this phone holds.
    @State private var inviteSheet: InviteSheetRequest?
    /// A pending invitation whose code this phone cannot show again.
    @State private var unavailableInvitation: PendingInvitation?
    /// An invitation from a link or the scanner, waiting for "Join {family}?"
    /// — shown once this phone is signed in, named, and not in a family.
    @State private var pendingInvitation: InvitationIntent?
    @State private var showingScanner = false
    /// "Scan QR code" found no usable camera, or the scanner would not start:
    /// said inline, above the paste field — with "Open Settings" when the
    /// camera was refused. Nil while there is nothing to say.
    @State private var scannerUnavailable: ScannerUnavailableNotice?
    /// An invitation link arrived: everything this screen has up gives way to
    /// its Join sheet or alert, and says when it has gone.
    @Environment(\.invitationLinkHandOff) private var linkHandOff
    @State private var handOffID = UUID()
    /// Held until the scanner has gone, so the confirmation is not presented
    /// on top of a cover that is still dismissing.
    @State private var scannedInvitation: InvitationIntent?
    @State private var pendingConfirmation: FamilyLifecycleConfirmation?
    @State private var isWorking = false
    @State private var showingServerSelection = false
    @State private var showingDisplayName = false

    private let hostedEndpoint: ConfirmedEndpoint?
    private let serverProbe: ServerSelectionView.Probe

    /// - Parameter invitation: an invitation opened from a link, taken
    ///   through the server, name and sign-in steps as needed and then to
    ///   "Join {family}?".
    public init(
        model: Model,
        invitation: InvitationIntent? = nil,
        hostedEndpoint: ConfirmedEndpoint? = FamilySyncComposition.hostedEndpoint,
        serverProbe: @escaping ServerSelectionView.Probe = { origin in
            try await HTTPIdentityClient().serverInfo(origin)
        }
    ) {
        self.model = model
        _pendingInvitation = State(initialValue: invitation)
        self.hostedEndpoint = hostedEndpoint
        self.serverProbe = serverProbe
    }

    public var body: some View {
        Form {
            switch model.mode {
            case .localOnly:
                serverSelectionSection
            case .endpointConfirmed:
                signedOutSection
            case .signedOut:
                revokedSection
            case .authenticated:
                noFamilySection
            case .recovery:
                recoverySection
            case .shared:
                if let line = model.syncStatusLine { syncStatusSection(line) }
                if model.isReadOnly { readOnlySection }
                FamilySharedSections(
                    model: model,
                    onInvite: { inviteSheet = .newInvitation },
                    onOpenInvitation: open,
                    onRevokeInvitation: { invitation in run { try await model.revokeInvitation(invitation.id) } },
                    onRetryInvitations: { Task { await model.reloadInvitations() } },
                    onConfirm: { pendingConfirmation = $0 }
                )
                Section {
                    NavigationLink("Account") { AccountSettingsView(model: model) }
                        .frame(minHeight: 44)
                        .accessibilityIdentifier("family.account")
                }
            }
        }
        .navigationTitle(FamilyCopy.title)
        // Members and invitations both change on other phones — a caregiver
        // joins, an invitation is redeemed — so a pull asks the server again.
        .refreshable { await reloadShared() }
        // Ownership handed to this phone while the screen is open brings
        // "Invite someone" with it, and the invitations that go with it.
        .task(id: [model.hasFamily, model.isOwner]) {
            if model.hasFamily, model.isOwner { await model.reloadInvitations() }
        }
        .navigationDestination(isPresented: $showingServerSelection) {
            ServerSelectionView(
                hostedEndpoint: hostedEndpoint,
                knownInstanceID: nil,
                prefilledOrigin: pendingInvitation?.origin,
                probe: serverProbe
            ) { endpoint in
                run {
                    try await model.confirmEndpoint(endpoint)
                    showingServerSelection = false
                }
            }
        }
        .navigationDestination(isPresented: $showingDisplayName) {
            DisplayNameConfirmationView(model: model) { showingDisplayName = false }
        }
        .disabled(isWorking)
        .sheet(item: $inviteSheet, onDismiss: dismissed) { request in
            InviteSheet(model: model, showing: request.invitation)
        }
        .sheet(item: $unavailableInvitation, onDismiss: dismissed) { invitation in
            UnavailableInvitationSheet(invitation: invitation) {
                run { try await model.revokeInvitation(invitation.id) }
            }
        }
        // A link opened before any server was chosen starts on the server
        // step, with the link's server filled in for the user to confirm.
        .task {
            if pendingInvitation != nil, case .localOnly = model.mode { showingServerSelection = true }
        }
        .scannerCover(isPresented: $showingScanner, onDismiss: {
            if let scannedInvitation { pendingInvitation = scannedInvitation }
            scannedInvitation = nil
            dismissed()
        }) {
            if let origin = model.confirmedOrigin {
                InvitationScannerView(confirmedOrigin: origin) { intent in
                    scannedInvitation = intent
                    showingScanner = false
                } onUnavailable: {
                    scannerUnavailable = ScannerUnavailableNotice(showSettings: CameraScanning.accessDenied)
                    showingScanner = false
                } onClose: {
                    showingScanner = false
                }
            }
        }
        .onChange(of: isPresenting) { _, presenting in
            if presenting { linkHandOff?.presenting(handOffID) }
        }
        .onDisappear { linkHandOff?.released(handOffID) }
        .onChange(of: model.mode) { old, new in
            guard !JoinConfirmationRequest.invitationSurvives(from: old, to: new) else { return }
            pendingInvitation = nil
            scannedInvitation = nil
        }
        .onChange(of: linkHandOff?.arrivals) { _, _ in
            showingScanner = false
            scannedInvitation = nil
            // This screen's own "Join {family}?" gives way to the link's.
            pendingInvitation = nil
            inviteSheet = nil
            unavailableInvitation = nil
            pendingConfirmation = nil
        }
        .sheet(item: joinConfirmation, onDismiss: dismissed) { request in
            JoinConfirmationView(model: model, intent: request.intent) { pendingInvitation = nil }
        }
        .sheet(item: $pendingConfirmation, onDismiss: dismissed) { confirmation in
            FamilyLifecycleConfirmationView(confirmation: confirmation) {
                pendingConfirmation = nil
                run(confirmation.action)
            } onCancel: {
                pendingConfirmation = nil
            }
        }
    }

    // MARK: Sections

    private var serverSelectionSection: some View {
        Section {
            Button(FamilyCopy.chooseServer) { showingServerSelection = true }
                .frame(minHeight: 44)
                .accessibilityIdentifier("family.chooseServer")
        } header: {
            Text(FamilyCopy.serverHeader)
        } footer: {
            Text(FamilyCopy.serverFooter)
        }
    }

    private var signedOutSection: some View {
        Section {
            // The name comes first: the server refuses an account's first
            // sign-in without one, and Apple only offers a name the very
            // first time — never on a second device or a reinstall.
            displayNameRow
            Button(FamilyCopy.signIn) { run { try await model.signInWithApple() } }
                .frame(minHeight: 44)
                .disabled(!model.displayNameConfirmed)
                .accessibilityIdentifier("family.signIn")
        } header: {
            Text(FamilyCopy.title)
        } footer: {
            // The reassurance that stops this screen reading as a paywall:
            // nothing here is required to use the app.
            Text(FamilyCopy.localOnlyFooter)
        }
    }

    /// Apple says this account no longer authorizes us. The screen states
    /// plainly that the family's data is safe and what the user must do.
    private var revokedSection: some View {
        Section {
            Text(model.confirmationCopy.isEmpty ? FamilyLifecycleCopy.selfHostRevocation : model.confirmationCopy)
                .font(.callout)
                .accessibilityIdentifier("family.revocationCopy")
            displayNameRow
            Button(FamilyCopy.signInAgain) { run { try await model.signInWithApple() } }
                .frame(minHeight: 44)
                .disabled(!model.displayNameConfirmed)
                .accessibilityIdentifier("family.signInAgain")
        } header: {
            Text(FamilyCopy.signedOutHeader)
        }
    }

    @ViewBuilder
    private var noFamilySection: some View {
        Section {
            // Apple gives a name on the first authorization only, and both
            // creating and joining need one. Without this row a reinstall or
            // a second device could do neither.
            displayNameRow
            TextField(FamilyCopy.familyNameField, text: $familyName)
                .frame(minHeight: 44)
                .accessibilityIdentifier("family.nameField")
            Button(FamilyCopy.createFamily) {
                run { try await model.createFamily(name: familyName) }
            }
            .frame(minHeight: 44)
            .disabled(familyName.trimmingCharacters(in: .whitespaces).isEmpty || !model.displayNameConfirmed)
            .accessibilityIdentifier("family.create")

            Button {
                Task {
                    // Asks for the camera the first time, so a fresh install
                    // can scan at all.
                    switch await CameraScanning.access() {
                    case .scan:
                        scannerUnavailable = nil
                        showingScanner = true
                    case .unavailable(let showSettings):
                        scannerUnavailable = ScannerUnavailableNotice(showSettings: showSettings)
                    case .requestAccess:
                        scannerUnavailable = ScannerUnavailableNotice(showSettings: false)
                    }
                }
            } label: {
                Label(FamilyCopy.scanQRCode, systemImage: "qrcode.viewfinder")
            }
            .frame(minHeight: 44)
            .disabled(!model.displayNameConfirmed || model.confirmedOrigin == nil)
            .accessibilityIdentifier("family.scanQRCode")
            if let scannerUnavailable {
                ScannerUnavailableRows(cameraDenied: scannerUnavailable.showSettings)
            }
        } header: {
            Text(FamilyCopy.noFamilyHeader)
        } footer: {
            Text(FamilyCopy.noFamilyFooter)
        }

        Section {
            InvitationCodeRows(model: model, isWorking: $isWorking)
        } header: {
            Text(FamilyCopy.orPasteCode)
        }
    }

    /// "Join {family}?" is up while an invitation waits and the phone can
    /// act on it: signed in, named, and not already in a family. Cancel,
    /// Close and a finished join all clear the invitation.
    private var joinConfirmation: Binding<JoinConfirmationRequest?> {
        Binding {
            guard let pendingInvitation, case .authenticated = model.mode, model.displayNameConfirmed else { return nil }
            return JoinConfirmationRequest(intent: pendingInvitation)
        } set: { request in
            if request == nil { pendingInvitation = nil }
        }
    }

    /// The name other caregivers see, asked for until it is confirmed and
    /// then shown, so it can be changed before it is sent.
    @ViewBuilder
    private var displayNameRow: some View {
        if model.displayNameConfirmed {
            Button {
                showingDisplayName = true
            } label: {
                LabeledContent(FamilyCopy.nameLabel, value: model.profile.displayName)
            }
            .frame(minHeight: 44)
            .accessibilityIdentifier("family.displayName")
        } else {
            Button(FamilyCopy.chooseName) { showingDisplayName = true }
                .frame(minHeight: 44)
                .accessibilityIdentifier("family.displayName")
        }
    }

    private var recoverySection: some View {
        Section {
            Text(FamilyCopy.recoveryExplanation)
                .font(.callout)
            Button(FamilyCopy.recoverFamily) { run { try await model.recoverFamily() } }
                .frame(minHeight: 44)
                .accessibilityIdentifier("family.recover")
            Button(FamilyCopy.forfeitRecovery, role: .destructive) {
                run { try await model.forfeitFamilyRecovery() }
            }
            .frame(minHeight: 44)
            .accessibilityIdentifier("family.forfeit")
        } header: {
            Text(FamilyCopy.recoveryHeader)
        }
    }

    /// Whether what was just logged will reach the other caregiver. Offline
    /// and a transient retry are said plainly and in the neutral colour: the
    /// entries are safe and nothing is asked of the parent.
    private func syncStatusSection(_ line: FamilySyncStatusLine) -> some View {
        Section {
            Text(line.text)
                .font(.callout)
                .foregroundStyle(line.isProblem ? Palette.primaryText : Palette.secondaryText)
                .frame(minHeight: 44, alignment: .leading)
                .accessibilityIdentifier("family.syncStatus")
            if line.offersSignIn {
                displayNameRow
                Button(FamilySyncStatusCopy.signInButton) { run { try await model.signInWithApple() } }
                    .frame(minHeight: 44)
                    .disabled(!model.displayNameConfirmed)
                    .accessibilityIdentifier("family.syncSignIn")
            }
        }
    }

    /// Says what read-only means and who can change it, once, in plain words.
    /// A viewer who does not know why the log buttons vanished will assume the
    /// app is broken.
    private var readOnlySection: some View {
        Section {
            Text(FamilyCopy.readOnlyExplanation)
                .font(.callout)
                .accessibilityIdentifier("family.readOnlyExplanation")
        } header: {
            Text(FamilyCopy.readOnlyHeader)
        }
    }

    // MARK: Plumbing

    /// A tapped pending invitation: the full sheet again if this phone kept
    /// its code, otherwise the sheet that can only revoke it.
    private func open(_ pending: PendingInvitation) {
        if let shareURL = model.shareURL(for: pending.id) {
            inviteSheet = .existing(
                FamilyInvitation(id: pending.id, shareURL: shareURL, role: pending.role, expiresAt: pending.expiresAt)
            )
        } else {
            unavailableInvitation = pending
        }
    }

    private func reloadShared() async {
        guard model.hasFamily else { return }
        do {
            try await model.reloadMembers()
        } catch {
            errorReporter.report(FamilyCopy.actionFailed, error: error, category: AppLog.sync)
        }
        if model.isOwner { await model.reloadInvitations() }
    }

    /// Anything this screen has presented, which a link's alert or Join sheet
    /// would have to wait for.
    private var isPresenting: Bool {
        showingScanner || inviteSheet != nil || unavailableInvitation != nil
            || pendingConfirmation != nil || joinConfirmation.wrappedValue != nil
    }

    /// Called from every presentation's `onDismiss`, once it has gone.
    private func dismissed() { if !isPresenting { linkHandOff?.released(handOffID) } }

    /// Every action funnels through here so a thrown error becomes the app's
    /// standard banner instead of a silently dead button.
    private func run(_ work: @escaping () async throws -> Void) {
        isWorking = true
        Task {
            defer { isWorking = false }
            do {
                try await work()
                errorReporter.clear()
            } catch {
                errorReporter.report(FamilyCopy.actionFailed, error: error, category: AppLog.store)
            }
        }
    }

}
