import SwiftUI
import BabyCore
import BabyStore
import BabyUI

enum HomeSyncCopy {
    static let signInAgain = HomeL10n.text("Sign in again to keep syncing")
    static let signInButton = HomeL10n.text("Sign in with Apple")
    static let queued = HomeL10n.text("Queued entries remain on this phone until syncing resumes.")
    static let offline = HomeL10n.text("Offline — this phone will retry syncing")
    static let failed = HomeL10n.text("Family sync needs attention. Please try again later.")
    static let signInFailed = HomeL10n.text("Could not sign in. Your queued entries remain on this phone.")
}

/// What Home asks the app layer to do. The app maps these onto its log sheets
/// and tabs, so `HomeFeature` does not depend on `LoggingFeature`.
public enum HomeAction: Equatable, Sendable {
    /// The Bottle sheet: start a timed bottle, or log what was drunk.
    case bottle
    case diaper
    /// The ··· sheet: Solids, Growth, Past entry and Meds.
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
    private let onSwitchBaby: () -> Void
    private let onAction: (HomeAction) -> Void
    /// A running bottle was stopped and needs its amount. Presented by the
    /// app layer with every other log sheet.
    private let onFinishBottle: (UUID) -> Void
    private let syncStatus: HomeSyncStatus
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
        onSwitchBaby: @escaping () -> Void,
        onAction: @escaping (HomeAction) -> Void,
        onFinishBottle: @escaping (UUID) -> Void,
        syncStatus: HomeSyncStatus = .hidden,
        onSignIn: (() async throws -> Void)? = nil,
        syncSignInTitle: String? = nil,
        syncSignInButton: String? = nil,
    ) {
        self.store = store
        self.babyName = babyName
        self.babyBirthDate = babyBirthDate
        self.babyAvatar = babyAvatar
        self.volumeUnit = volumeUnit
        self.onSwitchBaby = onSwitchBaby
        self.onAction = onAction
        self.onFinishBottle = onFinishBottle
        self.syncStatus = syncStatus
        self.onSignIn = onSignIn
        self.syncSignInTitle = syncSignInTitle ?? HomeSyncCopy.signInAgain
        self.syncSignInButton = syncSignInButton ?? HomeSyncCopy.signInButton
        _model = State(wrappedValue: HomeViewModel(store: store, babyID: babyID, volumeUnit: volumeUnit))
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
            ZStack(alignment: .bottom) {
                ScrollView {
                    VStack(spacing: 0) {
                        header
                        syncStatusCard
                            .padding(.horizontal, 20)
                            .padding(.top, 8)
                        glance
                            .padding(.horizontal, 20)
                            .padding(.top, 28)
                    }
                    // Standard text keeps the floating dock. At accessibility
                    // sizes it scrolls with the content, leaving the recovery
                    // card and every care action reachable.
                    .padding(.bottom, dynamicTypeSize.isAccessibilitySize ? 12 : 190)

                    if dynamicTypeSize.isAccessibilitySize {
                        dock
                    }
                }
                .accessibilityIdentifier("home.scrollView")

                if !dynamicTypeSize.isAccessibilitySize {
                    dock
                }
            }
            .background(backgroundGradient)
            .navigationTitle(babyName)
            #if os(iOS)
            // The name is drawn in the content as a tappable heading, so the
            // bar would render it a second time.
            .toolbar(.hidden, for: .navigationBar)
            #endif
        }
        .sheet(item: $timerEditor) { editor in
            RunningTimerEditSheet(model: editor) { timerEditor = nil }
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
        // A write from another process — the widget, an App Intent — does
        // not bump `revision`. Coming back to the app is the moment to re-read.
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { reload() }
        }
    }

    // MARK: - Layout

    /// The baby's name IS the switcher; the age sits opposite.
    private var header: some View {
        HStack(alignment: .center, spacing: 8) {
            BabySwitcherHeader(babyName: babyName, avatar: babyAvatar,
                               switcherID: "home.babySwitcher", headingID: "home.babyName",
                               onSwitchBaby: onSwitchBaby)
            Spacer(minLength: 0)
            if let babyBirthDate, let age = HomeCopy.age(birthDate: babyBirthDate, now: .now) {
                Text(age)
                    .font(.footnote)
                    .foregroundStyle(Palette.secondaryText)
                    .accessibilityIdentifier("home.age")
            }
        }
        .padding(.horizontal, 20)
        .padding(.top, 8)
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

    private var dock: some View {
        HomeDock(
            dock: screen.dock,
            onStartBreast: startBreast,
            onStartSleep: startSleep,
            onStopFeed: stopFeed,
            onWake: wake,
            onAction: onAction
        )
        .padding(.bottom, 12)
    }

    @ViewBuilder
    private var syncStatusCard: some View {
        if syncStatus != .hidden {
            VStack(alignment: .leading, spacing: 8) {
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
                    .buttonStyle(.borderedProminent)
                    .accessibilityIdentifier("home.syncSignIn")
                    .accessibilityLabel(syncSignInButton)
                }
                if signInFailed, syncStatus.offersSignIn {
                    Text(HomeSyncCopy.signInFailed)
                        .font(.footnote)
                        .foregroundStyle(Palette.primaryText)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(14)
            .background(.white.opacity(0.09), in: RoundedRectangle(cornerRadius: 14))
            .accessibilityElement(children: .contain)
            .accessibilityIdentifier("home.syncStatus")
        }
    }

    private var syncTitle: String {
        switch syncStatus {
        case .hidden: ""
        case .offline: HomeSyncCopy.offline
        case .needsSignIn: syncSignInTitle
        case .recoveryFailed: HomeSyncCopy.failed
        }
    }

    private var backgroundGradient: some View {
        RadialGradient(
            colors: [Color(red: 0.106, green: 0.145, blue: 0.278), Palette.ground],
            center: .top,
            startRadius: 0,
            endRadius: 700
        )
        .ignoresSafeArea()
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
        perform(ErrorSubject.feed) { try await model.startBreast(side) }
    }

    private func startSleep() {
        perform(ErrorSubject.sleep) { try await model.startSleep() }
    }

    private func stopFeed() {
        perform(ErrorSubject.feed) {
            if case .needsBottleAmount(let timerID) = try await model.stopFeed() {
                onFinishBottle(timerID)
            }
        }
    }

    private func wake() {
        perform(ErrorSubject.sleep) { try await model.stopSleep() }
    }

    private func detach(_ kind: SessionKind) {
        perform(kind == .feed ? ErrorSubject.feed : ErrorSubject.sleep) {
            try await model.detachRunning(kind)
        }
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
