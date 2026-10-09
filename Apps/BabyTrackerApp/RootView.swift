import SwiftUI
import Combine
import WidgetKit
import BabyCore
import BabyStore
import BabySync
import BabyUI
import BabyFeature
import HomeFeature
import HistoryFeature
import InsightsFeature
import LoggingFeature
import SettingsFeature
import AskFeature

struct RootView: View {
    /// Owned by `BabyTrackerApp`, which builds it at process launch.
    let environment: AppEnvironment
    @State private var babies: [Baby] = []
    @State private var babiesRealm: RealmID?
    @State private var sheet: LogSheet?
    @State private var isSwitchingBaby = false
    /// The Join sheet: from the first-launch screen, or from an invitation
    /// link, which fills in its invitation.
    @State private var joinSheet: JoinSheetRequest?
    /// An invitation link arrived while this phone is already in a family.
    @State private var isShowingAlreadyInFamily = false
    /// A link that arrived over a log sheet or the baby switcher, held until
    /// the parent closes it rather than throwing away a half-logged entry.
    @State private var queuedInvitation: InvitationRoute?
    /// Tells the Caregivers screen to clear its presentations for a link, and
    /// says when they have all finished dismissing.
    @State private var linkHandOff = InvitationLinkHandOff()
    /// Owned here rather than read from the environment, because this is the
    /// view that puts it there. One banner for the whole app.
    @State private var errorReporter = ErrorReporter()
    @State private var selectedTab: AppTab = .home
    @State private var isAskAvailable = false
    @State private var historyFocus: HistoryFocus?
    /// Home's running-timer editor, opened from a running row in History
    /// (ruling H-R5). Its own presentation: it is HomeFeature's sheet, not
    /// one of LoggingFeature's.
    @State private var timerEditor: RunningTimerEditModel?
    @Environment(\.scenePhase) private var scenePhase
    /// Where a tap outside the app parks what it leaves for the app to
    /// finish — the amount of a bottle stopped from the Lock Screen, or why a
    /// Lock Screen action was handed to the app instead of performed.
    private let pendingQuickLog = PendingQuickLogStore()
    /// A Lock Screen action could not be checked against the family's
    /// current state and wrote nothing: say so, once.
    @State private var isShowingActivityHandoff = false

    /// Keep the presentation alive when a menu choice becomes an editor.
    /// Changing `.sheet(item:)` to a different item dismisses and replaces the
    /// whole modal, which visibly flashes between Feed and Bottle/Solids.
    private var isLogSheetPresented: Binding<Bool> {
        Binding(
            get: { sheet != nil },
            set: { if !$0 { sheet = nil } }
        )
    }

    var body: some View {
        Group {
            if let store = environment.store {
                if let baby = environment.selection.resolve(from: babies) {
                    tabs(for: baby, store: store)
                        // Each tab owns a view model bound to a baby ID at init.
                        // Remount the tabs when that ID changes.
                        .id("\(baby.id.uuidString)-\(environment.askIdentity)")
                } else {
                    AddBabySheet(
                        store: store,
                        onAdded: { baby in
                            environment.selection.select(baby.id)
                            reload()
                        },
                        onJoinFamily: environment.familyModel.map { _ in { joinSheet = JoinSheetRequest(invitation: nil) } }
                    )
                    // The family's babies arrive with the joiner's snapshot.
                    // The tabs watch the revision themselves; this screen has
                    // to, or a caregiver who just joined would stay here.
                    .onChange(of: store.revision) { _, _ in reload() }
                }
            } else {
                // The store would not open. There is no partial app to show:
                // every screen reads from it. Retry reopens the same file and
                // nothing else — see `AppEnvironment.retry`.
                StoreFailureView(message: environment.storeFailure ?? StoreFailureCopy.title) {
                    environment.retry()
                    reload()
                }
            }
        }
        .sheet(item: $joinSheet, onDismiss: presentQueuedInvitation) { request in
            if let familyModel = environment.familyModel {
                JoinFamilySheet(model: familyModel, prefilled: request.invitation) { joinSheet = nil }
            }
        }
        .alert(InvitationLinkCopy.alreadyInFamilyTitle, isPresented: $isShowingAlreadyInFamily) {
            Button(InvitationLinkCopy.ok, role: .cancel) {}
        } message: {
            Text(InvitationLinkCopy.alreadyInFamily)
        }
        // The app's own foreground sync then refreshes the card; the tapped
        // action is not performed here either.
        .alert(LiveActivityHandoffCopy.message, isPresented: $isShowingActivityHandoff) {
            Button(LiveActivityHandoffCopy.ok, role: .cancel) {}
        }
        // Once the explanation is dismissed, whatever else was parked — a
        // bottle's "Set amount" — gets its turn.
        .onChange(of: isShowingActivityHandoff) { _, showing in
            if !showing { consumePendingQuickLog() }
        }
        // `babytracker://invite?…` — from the handoff page's "Open Baby
        // Tracker", or a QR code read by the Camera app. Routing decides;
        // nothing is redeemed until "Join {family}?" is confirmed.
        .onOpenURL { url in
            guard let familyModel = environment.familyModel else { return }
            // On the mode, not `hasFamily`: a family in recovery is still
            // this phone's family, and gets the alert.
            let route = InvitationRouting.route(url: url, mode: familyModel.mode)
            guard route != .ignore else { return }
            // Read before `linkArrived()`: whatever the Caregivers screen has
            // up is dismissed by it, and the link waits for that to finish.
            let caregiversBusy = linkHandOff.isBusy
            linkHandOff.linkArrived()
            if sheet != nil || timerEditor != nil || isSwitchingBaby || caregiversBusy {
                queuedInvitation = route
            } else if joinSheet != nil, route == .alreadyInFamily {
                // Joined from this sheet a moment ago: it closes first, and
                // its `onDismiss` shows the alert.
                queuedInvitation = route
                joinSheet = nil
            } else {
                present(route)
            }
        }
        // The Caregivers screen's presentations have all finished going.
        .onChange(of: linkHandOff.isBusy) { _, busy in if !busy { presentQueuedInvitation() } }
        .overlay(alignment: .top) {
            if let message = errorReporter.current {
                ErrorBanner(message: message) { errorReporter.dismiss() }
                    .transition(.move(edge: .top).combined(with: .opacity))
            }
        }
        .animation(.snappy, value: errorReporter.current)
        .environment(\.errorReporter, errorReporter)
        .environment(\.invitationLinkHandOff, linkHandOff)
        .preferredColorScheme(.dark)
        .task {
            reload()
            refreshAskAvailability()
            // A Lock Screen tap may have parked something before this view
            // existed. Read here only if the scene is already active; a
            // background launch leaves it for `.active` below.
            consumePendingQuickLog()
            // A launch straight into the foreground may never report a change
            // to `.active`, so the cards are reconciled here too. Before the
            // sync: where the server pushes to the cards, any card with server
            // content is left for the sync engine's pass after a successful
            // sync, so this snapshot cannot replace a newer push.
            if let store = environment.store {
                await LiveActivityController.reconcileBeforeSync(in: store)
            }
            #if DEBUG
            UITestSupport.seedErrorBannerIfRequested(errorReporter)
            openRequestedLogSheetIfNeeded()
            #endif
            // Last, because it waits on the network. Does nothing for a device
            // that is not in a family.
            await environment.familyModel?.startSyncing()
        }
        // …and it may be parked after this view existed, from a cold launch
        // whose intent ran late, or while the app sat in the background.
        .onChange(of: scenePhase) { _, phase in
            switch phase {
            case .active:
                refreshAskAvailability()
                consumePendingQuickLog()
                // A break card cannot take itself down when its limit
                // passes — nothing runs then. This is the next chance (a
                // pushed card: the server's own expiry, or the pass after
                // the sync below).
                if let store = environment.store {
                    Task { await LiveActivityController.reconcileBeforeSync(in: store) }
                }
                Task { await environment.familyModel?.sceneBecameActive() }
            case .background:
                // Not on `.inactive`: pulling down Control Center is not
                // leaving the app, and polling should not stutter for it.
                environment.familyModel?.sceneEnteredBackground()
            default:
                break
            }
        }
        // An app already on screen never changes scene phase, so neither of
        // the two above would fire for it. Only while on screen: the Lock
        // Screen's bottle Stop runs in this process with the app in the
        // background, and its "Set amount" must wait for the parent to open
        // the app rather than be consumed by a sheet nobody sees.
        .onReceive(NotificationCenter.default.publisher(for: PendingQuickLogStore.didRequest)) { _ in
            consumePendingQuickLog()
        }
        .onChange(of: environment.selection.selectedID) { _, _ in
            WidgetCenter.shared.reloadAllTimelines()
        }
        // The sync's own remote apply reloads while the engine still says
        // `syncing`, when assigning is held back; the success comes after.
        .onChange(of: familyRowsAreCurrent) { _, current in
            if current, let store = environment.store { fillInBabyIcons(store) }
        }
        .onChange(of: environment.askIdentity) { old, new in
            do {
                switch (old, new) {
                case (.local, .account):
                    environment.markAskAccountEntered(new)
                    try environment.promoteAllLocalAskConversations(to: new)
                default: break
                }
            }
            catch { errorReporter.report("Could not move Ask chat", error: error, category: AppLog.store) }
        }
    }

    private func tabs(for baby: Baby, store: BabyStore) -> some View {
        TabView(selection: $selectedTab) {
            Tab("Home", systemImage: "house.fill", value: .home) {
                HomeView(
                    store: store,
                    babyID: baby.id,
                    babyName: baby.name,
                    babyBirthDate: baby.birthDate,
                    babyAvatar: BabyAvatar(iconID: baby.iconID),
                    volumeUnit: environment.volumeUnit,
                    onSwitchBaby: { isSwitchingBaby = true },
                    onAction: handleHomeAction,
                    // A running bottle was stopped: its amount is recorded as
                    // the timer finishes.
                    onFinishBottle: { timerID in sheet = .bottleStopped(timerID: timerID) },
                    syncStatus: homeSyncStatus,
                    onSignIn: environment.familyModel.map { model in { try await model.signInWithApple() } },
                    syncSignInTitle: FamilySyncStatusCopy.signInAgain,
                    syncSignInButton: FamilySyncStatusCopy.signInButton
                )
            }
            Tab("History", systemImage: "list.bullet", value: .history) {
                HistoryView(
                    store: store,
                    babyID: baby.id,
                    babyName: baby.name,
                    babyAvatar: BabyAvatar(iconID: baby.iconID),
                    volumeUnit: environment.volumeUnit,
                    focus: historyFocus,
                    onFocusHandled: { historyFocus = nil },
                    onSwitchBaby: { isSwitchingBaby = true },
                    onAction: { handleHistoryAction($0, store: store, babyID: baby.id) }
                )
            }
            if isAskAvailable {
                Tab(AskCopy.title, systemImage: "sparkles", value: .ask) {
                    AskView(
                        model: AskViewModel(
                            store: store, conversationStore: environment.askConversations,
                            model: environment.askModel, identity: environment.askIdentity,
                            babyID: baby.id, babyName: baby.name, birthDate: baby.birthDate,
                            volumeUnit: environment.volumeUnit
                        ),
                        babyName: baby.name,
                        birthDate: baby.birthDate,
                        volumeUnit: environment.volumeUnit,
                        onOpenSource: { source in
                            switch source {
                            case .runningTimer:
                                selectedTab = .home
                            case .entry(_, let id):
                                historyFocus = HistoryFocus(entryID: id)
                                selectedTab = .history
                            case .insight, .insightRange:
                                break
                            }
                        }
                    )
                }
            }
            Tab("Insights", systemImage: "chart.bar.fill", value: .insights) {
                InsightsView(
                    store: store, babyID: baby.id, babyName: baby.name,
                    babyAvatar: BabyAvatar(iconID: baby.iconID), volumeUnit: environment.volumeUnit,
                    onSwitchBaby: { isSwitchingBaby = true },
                    onOpenAllCheckIns: { historyFocus = HistoryFocus(filter: [.growth]); selectedTab = .history }
                )
            }
            // A fourth tab, against the earlier note that the tab bar is only
            // for the three things a parent logs. A gear floating in Home's
            // top-right was competing with the baby switcher for the same
            // corner, and settings is a destination rather than an action.
            Tab("Settings", systemImage: "gearshape.fill", value: .settings) {
                SettingsView(
                    store: store,
                    selection: environment.selection,
                    settings: environment.settings,
                    familyModel: environment.familyModel,
                    onBabyDeleted: { id in
                        do { try environment.askConversations.removeBaby(id) }
                        catch { errorReporter.report("Could not remove Ask chat", error: error, category: AppLog.store) }
                    }
                )
            }
        }
        // Settings used to hand back a Done that reloaded this list. It is a
        // tab now with no way to say it finished, so the list follows the
        // store instead: every rename, add and delete in there goes through
        // `store.record`, which bumps `revision`.
        .onChange(of: store.revision) { _, _ in
            reload()
            // The engine pushes only if something is actually owed, so a
            // remote apply bumping the revision costs nothing.
            environment.familyModel?.localWriteHappened()
            // A sync may have applied the other phone's stop under the editor.
            timerEditor?.refreshAvailability()
        }
        .sheet(isPresented: isLogSheetPresented, onDismiss: presentQueuedInvitation) {
            if let current = sheet {
                LogSheetView(
                    sheet: current,
                    store: store,
                    babyID: baby.id,
                    unit: environment.volumeUnit,
                    onDone: { sheet = nil },
                    onSwitchTo: { sheet = $0 }
                )
                // Preserve each editor's seed and form state without replacing
                // the modal presentation that contains it.
                .id(current.id)
            }
        }
        .sheet(item: $timerEditor, onDismiss: presentQueuedInvitation) { editor in
            RunningTimerEditSheet(model: editor) { timerEditor = nil }
        }
        .sheet(isPresented: $isSwitchingBaby, onDismiss: presentQueuedInvitation) {
            BabySwitcherSheet(
                store: store,
                selection: environment.selection,
                onDone: {
                    isSwitchingBaby = false
                    reload()
                }
            )
        }
    }

    /// Home's dock and today strip, mapped onto this app's sheets and tabs.
    private func handleHomeAction(_ action: HomeAction) {
        switch action {
        case .bottle: sheet = .bottle
        case .diaper: sheet = .diaper
        case .more: sheet = .more
        case .history: selectedTab = .history
        }
    }

    /// History's taps, mapped onto LoggingFeature's sheets, or onto Home's
    /// running-timer editor for a running row.
    private func handleHistoryAction(_ action: HistoryAction, store: BabyStore, babyID: UUID) {
        switch action {
        case .edit(let ref):
            sheet = .editEntry(ref)
        case let .session(kind, rootID):
            sheet = .session(kind, rootID: rootID)
        case .runningTimer(let id):
            do {
                // Stopped since the list last loaded: there is nothing left
                // to correct, and the list is about to drop the row.
                guard let timer = try store.activeTimers(for: babyID).first(where: { $0.id == id }) else { return }
                timerEditor = RunningTimerEditModel(timer: timer, store: store, unit: environment.volumeUnit)
            } catch {
                errorReporter.report(ErrorBannerCopy.loadFailed, error: error, category: AppLog.store)
            }
        }
    }

    private var homeSyncStatus: HomeSyncStatus {
        guard let model = environment.familyModel, case .shared = model.mode else { return .hidden }
        guard let count = model.pendingDeliveryCount else { return .recoveryFailed("store") }
        let phase: HomeSyncPhase
        switch model.syncEngineStatus ?? .idle {
        case .idle: phase = .idle
        case .syncing: phase = .syncing
        case .upToDate(let date): phase = .current(date)
        case .offline: phase = .offline
        case .needsSignIn: phase = .needsSignIn
        case .failed(let message): phase = message == SyncIssueCopy.retrying ? .renewing : .failed(message)
        }
        return HomeSyncStatus.resolve(phase, pendingDeliveryCount: count)
    }

    private func refreshAskAvailability() {
        let available = environment.askModel.availability(for: .current) == .available
        if !available && selectedTab == .ask {
            selectedTab = .home
        }
        isAskAvailable = available
    }

    /// A link that waited for something on screen to go: a log sheet, the
    /// baby switcher or the Join sheet (from that sheet's `onDismiss`), or the
    /// Caregivers screen's presentations (`InvitationLinkHandOff.isBusy`
    /// turning false). Both fire once the presentation has actually gone.
    private func presentQueuedInvitation() {
        guard let route = queuedInvitation, !linkHandOff.isBusy else { return }
        queuedInvitation = nil
        present(route)
    }

    private func present(_ route: InvitationRoute) {
        switch route {
        case .ignore:
            break
        case .alreadyInFamily:
            isShowingAlreadyInFamily = true
        case .join(let intent):
            joinSheet = JoinSheetRequest(invitation: intent)
        }
    }

    /// A tap parked outside the app, acted on once. Reading it clears it, so
    /// a launch for any other reason does not re-open a sheet.
    ///
    /// Only while the scene is active, from every caller. The Lock Screen's
    /// bottle Stop runs in this process with no UI on screen — resuming a
    /// suspended app, or launching it in the background, where iOS may still
    /// connect the scene and run `.task`. Consuming then would set a sheet on
    /// a scene nobody sees, which iOS may disconnect before the parent ever
    /// opens the app: the prompt lost, the marker already cleared. Left
    /// parked, it is read at the next `.active`.
    ///
    /// One thing at a time: a parked handoff is explained first, and the
    /// bottle's "Set amount" follows once that alert is dismissed
    /// (`PendingQuickLogStore.consumeNextActivation`) — an alert and a sheet
    /// presented on the same activation would fight over the screen. Nothing
    /// is consumed while that alert is still up (a bottle Stop on the Lock
    /// Screen posts `didRequest` into the running app); the `onChange` on
    /// `isShowingActivityHandoff` reads it once the alert goes.
    private func consumePendingQuickLog() {
        guard scenePhase == .active else { return }
        switch pendingQuickLog.consumeNextActivation(whileShowingHandoff: isShowingActivityHandoff) {
        case .activityHandoff:
            isShowingActivityHandoff = true
        case .quickLog(let request):
            presentPendingRequest(request)
        case nil:
            break
        }
    }

    private func presentPendingRequest(_ request: PendingQuickLogRequest) {
        if let babyID = request.babyID {
            guard let store = environment.store,
                  (try? store.baby(babyID)) != nil else { return }
            environment.selection.select(babyID)
            reload()
        }
        switch request.action {
        case .bottleAmount(let feedID):
            // A timed bottle ended outside the app was saved at the usual
            // amount; Home offers to set the real one.
            selectedTab = .home
            sheet = .bottleAmount(feedID: feedID)
        }
    }

    #if DEBUG
    /// Opens a sheet directly at launch, for screenshotting it without a
    /// tap — the simulator has no UI-automation path available here. Each
    /// flag names the sheet verification needs to see; none is read outside
    /// a debug build.
    private func openRequestedLogSheetIfNeeded() {
        guard let store = environment.store,
              let baby = environment.selection.resolve(from: babies) else { return }
        let arguments = ProcessInfo.processInfo.arguments
        if arguments.contains("-uiTestOpenDiaper") {
            sheet = .diaper
        } else if arguments.contains("-uiTestOpenBottle") {
            sheet = .bottle
        } else if arguments.contains("-uiTestOpenBottleStopped"),
                  let timer = try? store.activeTimer(kind: .feed, for: baby.id) {
            sheet = .bottleStopped(timerID: timer.id)
        } else if arguments.contains("-uiTestOpenMore") {
            sheet = .more
        } else if arguments.contains("-uiTestOpenPastEntry") {
            sheet = .pastEntry
        }
    }
    #endif

    /// True once a family sync has succeeded in this process (and the last
    /// attempt did too): the rows then hold the server's latest, so a recorded
    /// icon assignment cannot send a stale rename back. The engine is new per
    /// launch and per family, so `upToDate` never survives from before.
    private var familyRowsAreCurrent: Bool {
        if case .upToDate = environment.familyModel?.syncEngineStatus { return true }
        return false
    }

    /// Older babies, and babies a sync brought in, get their icon here: the
    /// server copy's first, then — in a family only after a sync —
    /// assigned. Logged, not reported: a read-only family would otherwise
    /// show a banner on every reload. A write bumps the revision, which
    /// reloads the list.
    private func fillInBabyIcons(_ store: BabyStore) {
        do { try store.assignMissingBabyIcons(sharedRealmSynced: familyRowsAreCurrent) }
        catch { AppLog.store.error("Baby icon fill-in skipped: \(String(describing: type(of: error)), privacy: .public)") }
    }

    private func reload() {
        guard let store = environment.store else {
            babies = []
            return
        }
        do {
            let previous = Set(babies.map(\.id))
            let previousRealm = babiesRealm
            fillInBabyIcons(store)
            babies = try store.babies()
            babiesRealm = store.activeRealm
            // A remote tombstone can arrive while this process is closed, so
            // there may be no previously visible baby to compare against.
            do {
                for id in try environment.askConversations.babyIDs(identity: environment.askIdentity) {
                    if try store.baby(id)?.isTombstoned == true {
                        try environment.askConversations.clear(identity: environment.askIdentity, babyID: id)
                    }
                }
            } catch {
                errorReporter.report("Could not remove Ask chat", error: error, category: AppLog.store)
            }
            if previousRealm == store.activeRealm {
                for removed in previous.subtracting(Set(babies.map(\.id))) {
                    do {
                        guard try store.baby(removed)?.isTombstoned == true else { continue }
                        do { try environment.askConversations.removeBaby(removed) }
                        catch { errorReporter.report("Could not remove Ask chat", error: error, category: AppLog.store) }
                    } catch {
                        errorReporter.report("Could not check baby removal", error: error, category: AppLog.store)
                    }
                }
            }
            // A join that brought the family's babies is finished: the Join
            // sheet closes onto the tabs. Not on babies alone — an invitation
            // link can open the sheet over a phone that already has local
            // babies, and it must stay up until that join happens.
            if !babies.isEmpty, environment.familyModel?.hasFamily == true { joinSheet = nil }
            // Only a stale load banner is cleared here. A save failure's
            // banner must survive an unrelated successful read: a parent who
            // sees it vanish assumes the entry went in and does not re-log it.
            if errorReporter.current == ErrorBannerCopy.loadFailed { errorReporter.clear() }
        } catch {
            babies = []
            errorReporter.report(
                ErrorBannerCopy.loadFailed,
                error: error,
                category: AppLog.store
            )
        }
    }
}

private enum AppTab: Hashable {
    case home, history, ask, insights, settings
}

/// Why the Join sheet is up, and with which invitation, if any. A fresh id
/// per request, so a second link replaces the sheet rather than being lost.
private struct JoinSheetRequest: Identifiable {
    let id = UUID()
    let invitation: InvitationIntent?
}
