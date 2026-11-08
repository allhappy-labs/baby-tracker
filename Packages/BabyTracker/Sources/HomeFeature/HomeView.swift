import SwiftUI
import BabyCore
import BabyStore
import BabyUI

enum HomeSyncCopy {
    static let signInAgain = HomeL10n.text("Sign in again to keep syncing")
    static let signInButton = HomeL10n.text("Sign in with Apple")
    static let queued = HomeL10n.text("Queued entries remain on this phone until syncing resumes.")
    static let offline = HomeL10n.text("Offline — this phone will retry syncing")
    static let serverUnreachable = HomeL10n.text("Can't reach the family server")
    static let failed = HomeL10n.text("Family sync needs attention. Please try again later.")
    static let signInFailed = HomeL10n.text("Could not sign in. Your queued entries remain on this phone.")
}

/// What Home asks the app layer to do. The app maps these onto its log sheets
/// and tabs, so `HomeFeature` does not depend on `LoggingFeature`.
public enum HomeAction: Equatable, Sendable {
    /// The Bottle sheet: start a timed bottle, or log what was drunk.
    case bottle
    case diaper
    case solids
    /// The Meds panel: the saved meds as chips, and "+".
    case meds
    case growth
    /// Opens the vaccine panel.
    case vaccine
    /// Opens the pump panel: start a session, or edit the one running.
    case pump
    /// The pump strip's tap: the running session in the pump panel.
    case editPump(UUID)
    /// The pump strip's Stop: the session ended and needs its amount.
    case pumpStopped(UUID)
    /// The ··· sheet: whatever the dock has no room for, and Past entry.
    case more
    /// History at today, from the today strip.
    case history

    /// The ··· control's name, for whatever the app presents behind it.
    public static var moreTitle: String { HomeCopy.more }
}

/// Glance on top, dock in the thumb zone.
public struct HomeView: View {
    @State private var model: HomeViewModel
    private let store: BabyStore
    private let babyName: String
    private let babyBirthDate: Date?
    private let babyAvatar: BabyAvatar
    /// Kept as well as handed to the model, so a change to the preference can
    /// be noticed while this screen is alive.
    private let volumeUnit: VolumeUnit
    /// The order of this baby's Home buttons: a preference, as the unit is.
    private let homeOrder: [HomeActionKind]
    private let onSwitchBaby: () -> Void
    private let onAction: (HomeAction) -> Void
    /// A running bottle was stopped and needs its amount. Presented by the
    /// app layer with every other log sheet.
    private let onFinishBottle: (UUID) -> Void
    private let syncStatus: HomeSyncStatus
    /// While set, the store may predate the other phone's last taps: a timer
    /// verb waits for it (see `HomeViewModel.onCurrentState`).
    private let awaitCurrentState: (() async -> Void)?
    private let onSignIn: (() async throws -> Void)?
    private let syncSignInTitle: String
    private let syncSignInButton: String
    @State private var signInFailed = false

    /// Whether Home is on screen. With the scene phase, it decides whether
    /// the once-a-minute refresh should run at all.
    @State private var isVisible = false

    /// The open running-timer editor, if any. Held here rather than built in
    /// the sheet's closure, so a redraw underneath never throws edits away.
    @State private var timerEditor: RunningTimerEditModel?

    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Environment(\.scenePhase) private var scenePhase
    @Environment(\.errorReporter) private var errorReporter

    public init(
        store: BabyStore,
        babyID: UUID,
        babyName: String,
        babyBirthDate: Date? = nil,
        babyAvatar: BabyAvatar = .neutral,
        volumeUnit: VolumeUnit = .millilitres,
        homeOrder: [HomeActionKind] = HomeActionKind.defaultOrder,
        onSwitchBaby: @escaping () -> Void,
        onAction: @escaping (HomeAction) -> Void,
        onFinishBottle: @escaping (UUID) -> Void,
        syncStatus: HomeSyncStatus = .hidden,
        awaitCurrentState: (() async -> Void)? = nil,
        onSignIn: (() async throws -> Void)? = nil,
        syncSignInTitle: String? = nil,
        syncSignInButton: String? = nil,
    ) {
        self.store = store
        self.babyName = babyName
        self.babyBirthDate = babyBirthDate
        self.babyAvatar = babyAvatar
        self.volumeUnit = volumeUnit
        self.homeOrder = homeOrder
        self.onSwitchBaby = onSwitchBaby
        self.onAction = onAction
        self.onFinishBottle = onFinishBottle
        self.syncStatus = syncStatus
        self.awaitCurrentState = awaitCurrentState
        self.onSignIn = onSignIn
        self.syncSignInTitle = syncSignInTitle ?? HomeSyncCopy.signInAgain
        self.syncSignInButton = syncSignInButton ?? HomeSyncCopy.signInButton
        _model = State(wrappedValue: HomeViewModel(store: store, babyID: babyID, volumeUnit: volumeUnit, homeOrder: homeOrder))
    }

    private var screen: HomeScreen { model.screen }

    private var canLog: Bool { screen.dock != .readOnly }

    /// Something is running, so the hero is a part whose start can be edited.
    private var isRunning: Bool {
        switch screen.hero {
        case .feedRunning, .sleepRunning: true
        case .feedBreak, .awake: false
        }
    }

    /// Whether the once-a-minute refresh runs: only while Home is on screen
    /// and the app is active. It catches a break closing and the day rolling
    /// over, which no write announces — the today strip shows in every state,
    /// so a timer running past midnight needs it too.
    private var refreshesEveryMinute: Bool {
        isVisible && scenePhase == .active
    }

    public var body: some View {
        NavigationStack {
            // The scaffold draws the ground and builds the switcher header;
            // Home places the header inside its own scroll view, so it
            // scrolls away as before. Home owns the scrolling because the
            // dock floats outside the scroll view (glass must not scroll
            // under glass) except at the accessibility sizes, where it
            // scrolls with the content.
            ScreenScaffold(
                header: .baby(name: babyName, avatar: babyAvatar, switcherID: "home.babySwitcher",
                              headingID: "home.babyName", onSwitch: onSwitchBaby)
            ) {
                age
            } hostingHeader: { header in
                ZStack(alignment: .bottom) {
                    ScrollView {
                        VStack(spacing: 0) {
                            // No top padding: the row sits where the other tabs'
                            // pinned header rows do.
                            header
                            syncStatusCard
                                .padding(.horizontal, Spacing.l)
                                .padding(.top, Spacing.s)
                            glance
                                .padding(.horizontal, Spacing.l)
                                .padding(.top, 28)
                        }
                        // Standard text keeps the floating dock. At accessibility
                        // sizes it scrolls with the content, leaving the recovery
                        // card and every care action reachable.
                        .padding(.bottom, dynamicTypeSize.isAccessibilitySize ? 12 : (screen.pump == nil ? 198 : 258))

                        if dynamicTypeSize.isAccessibilitySize {
                            dockArea
                        }
                    }
                    .accessibilityIdentifier("home.scrollView")

                    if !dynamicTypeSize.isAccessibilitySize {
                        dockArea
                    }
                }
            }
            .navigationTitle(babyName)
            #if os(iOS)
            // The name is drawn in the content as a tappable heading, so the
            // bar would render it a second time.
            .toolbar(.hidden, for: .navigationBar)
            #endif
        }
        .fittedSheet(item: $timerEditor) { editor in
            RunningTimerEditSheet(model: editor) { timerEditor = nil }
                // A fitted sheet does not inherit custom environment values.
                .environment(\.errorReporter, errorReporter)
        }
        .onAppear {
            isVisible = true
            reload()
        }
        .onDisappear { isVisible = false }
        // Once a minute at most. Never per second: the hero, the chips, the
        // status line and the strip's running parts tick from their own
        // timelines.
        .task(id: refreshesEveryMinute) {
            guard refreshesEveryMinute else { return }
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(60))
                guard !Task.isCancelled else { return }
                reload()
            }
        }
        // A write is the event that makes Home stale: a part started or
        // stopped here, on the other phone, or by a sheet.
        .onChange(of: store.revision) { _, _ in
            reload()
            // A sync may have applied the other phone's stop under the editor.
            timerEditor?.refreshAvailability()
        }
        .onChange(of: volumeUnit) { _, unit in
            model.volumeUnit = unit
        }
        .onChange(of: homeOrder) { _, order in
            model.homeOrder = order
            reload()
        }
        // A write from another process — the widget, an App Intent — does
        // not bump `revision`. Coming back to the app is the moment to re-read.
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { reload() }
        }
    }

    // MARK: - Layout

    /// The baby's name IS the switcher (the scaffold's header); the age sits
    /// opposite.
    @ViewBuilder
    private var age: some View {
        if let babyBirthDate, let age = HomeCopy.age(birthDate: babyBirthDate, now: .now) {
            Text(age)
                .font(.footnote)
                .foregroundStyle(Palette.secondaryText)
                .accessibilityIdentifier("home.age")
        }
    }

    /// Only a caregiver who may write can detach or edit a running part.
    private var detachAction: ((SessionKind) -> Void)? {
        guard canLog else { return nil }
        return { detach($0) }
    }

    private var editAction: (() -> Void)? {
        guard canLog, isRunning else { return nil }
        return { editHeroTimer() }
    }

    private var glance: some View {
        VStack(spacing: 16) {
            if let chip = screen.joinChip {
                JoinChipView(chip: chip, onDetach: detachAction)
            }
            HeroView(hero: screen.hero, onEdit: editAction)
            if !screen.status.isEmpty {
                StatusLineView(items: screen.status)
            }
            if let sleepClock = screen.sleepChip {
                SleepChipView(clockFrom: sleepClock)
            }
            if let strip = screen.strip {
                TodayStripView(strip: strip, volumeUnit: volumeUnit) { onAction(.history) }
                    .padding(.top, 14)
            }
        }
    }

    /// The pump strip, when a pump runs, sits on the dock. It is a separate
    /// view: the dock's feed and sleep controls never change because of it.
    private var dockArea: some View {
        VStack(spacing: Spacing.s) {
            if let pump = screen.pump {
                PumpStripView(pump: pump, canLog: canLog,
                              onOpen: { onAction(.editPump(pump.id)) },
                              onStop: stopPump)
                    .padding(.horizontal, Spacing.l)
            }
            dock
        }
    }

    private var dock: some View {
        HomeDock(
            dock: screen.dock,
            onStartBreast: startBreast,
            onStartSleep: startSleep,
            onStopFeed: stopFeed,
            onWake: wake,
            onAction: onAction
        )
        .padding(.bottom, Spacing.m)
    }

    @ViewBuilder
    private var syncStatusCard: some View {
        if syncStatus != .hidden {
            Card {
                VStack(alignment: .leading, spacing: Spacing.s) {
                    Text(syncTitle)
                        .font(.subheadline.weight(.semibold))
                        .foregroundStyle(Palette.primaryText)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("home.syncTitle")
                    if syncStatus.showsQueuedExplanation {
                        Text(HomeSyncCopy.queued)
                            .font(.footnote)
                            .foregroundStyle(Palette.secondaryText)
                            .fixedSize(horizontal: false, vertical: true)
                            .accessibilityIdentifier("home.syncQueued")
                    }
                    if syncStatus.offersSignIn, let onSignIn {
                        Button {
                            Task {
                                do { try await onSignIn(); signInFailed = false }
                                catch { signInFailed = true }
                            }
                        } label: {
                            Text(syncSignInButton)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        .buttonStyle(.primary)
                        .accessibilityIdentifier("home.syncSignIn")
                        .accessibilityLabel(syncSignInButton)
                    }
                    if signInFailed, syncStatus.offersSignIn {
                        Text(HomeSyncCopy.signInFailed)
                            .font(.footnote)
                            .foregroundStyle(Palette.primaryText)
                    }
                }
            }
            .accessibilityElement(children: .contain)
            .accessibilityIdentifier("home.syncStatus")
        }
    }

    private var syncTitle: String {
        switch syncStatus {
        case .hidden: ""
        case .offline: HomeSyncCopy.offline
        case .serverUnreachable: HomeSyncCopy.serverUnreachable
        case .needsSignIn: syncSignInTitle
        case .recoveryFailed: HomeSyncCopy.failed
        }
    }

    // MARK: - Loading

    /// The one load path, matching History and Insights. A read failure says
    /// so rather than leaving stale figures with nothing to explain them.
    private func reload() {
        do {
            try model.refresh()
            // Only a stale load banner is cleared. A save failure's banner must
            // survive a successful read: a parent who sees it vanish assumes
            // the entry went in and does not re-log it.
            if errorReporter.current == ErrorBannerCopy.loadFailed { errorReporter.clear() }
        } catch {
            errorReporter.report(ErrorBannerCopy.loadFailed, error: error, category: AppLog.store)
        }
    }

    // MARK: - Verbs

    private func editHeroTimer() {
        timerEditor = model.makeTimerEditor()
    }

    private func startBreast(_ side: BreastSide) {
        performOnCurrentState(ErrorSubject.feed) { try await model.startBreast(side) }
    }

    private func startSleep() {
        performOnCurrentState(ErrorSubject.sleep) { try await model.startSleep() }
    }

    private func stopFeed() {
        performOnCurrentState(ErrorSubject.feed) {
            if case .needsBottleAmount(let timerID) = try await model.stopFeed() {
                onFinishBottle(timerID)
            }
        }
    }

    private func wake() {
        performOnCurrentState(ErrorSubject.sleep) { try await model.stopSleep() }
    }

    private func stopPump() {
        performOnCurrentState(ErrorSubject.pump) {
            if let id = try await model.stopPump() { onAction(.pumpStopped(id)) }
        }
    }

    private func detach(_ kind: SessionKind) {
        performOnCurrentState(kind == .feed ? ErrorSubject.feed : ErrorSubject.sleep) {
            try await model.detachRunning(kind)
        }
    }

    /// A timer verb, run only once the store has caught up and only if the
    /// catch-up left its target as the parent saw it.
    private func performOnCurrentState(_ subject: String, _ work: @escaping () async throws -> Void) {
        let catchUp = awaitCurrentState
        perform(subject) { try await model.onCurrentState(catchUp: catchUp, work) }
    }

    /// Runs a verb, then re-reads. There is no sheet to hold a failure, so the
    /// banner is the only thing that can say the tap did not take.
    private func perform(_ subject: String, _ work: @escaping () async throws -> Void) {
        Task {
            do {
                try await work()
                errorReporter.clear()
            } catch {
                errorReporter.report(ErrorBannerCopy.saveFailed(what: subject), error: error, category: AppLog.store)
            }
            reload()
        }
    }
}
