import SwiftUI
import BabyCore
import BabyStore
import BabyUI
import GrowthFeature

public struct InsightsView: View {
    /// Stored so the view can observe `store.revision`; the view model reads
    /// through its own reference.
    private let store: BabyStore
    private let babyID: UUID
    /// Kept as well as handed to the model, so a change to the preference can
    /// be noticed while this screen is alive.
    private let volumeUnit: VolumeUnit
    /// Shown in the pinned header. A screen of numbers about a baby should
    /// say which baby, and with more than one in the family it is the only
    /// thing telling these figures apart from the other child's.
    private let babyName: String
    private let babyAvatar: BabyAvatar
    private let onSwitchBaby: (() -> Void)?
    private let onOpenAllCheckIns: () -> Void
    @State private var model: InsightsViewModel
    @State private var range: InsightRange = .week
    @Environment(\.errorReporter) private var errorReporter
    @Environment(\.scenePhase) private var scenePhase

    public init(
        store: BabyStore,
        babyID: UUID,
        babyName: String = "",
        babyAvatar: BabyAvatar = .neutral,
        volumeUnit: VolumeUnit = .millilitres,
        onSwitchBaby: (() -> Void)? = nil,
        onOpenAllCheckIns: @escaping () -> Void = {}
    ) {
        self.babyID = babyID
        self.store = store
        self.volumeUnit = volumeUnit
        self.babyName = babyName
        self.babyAvatar = babyAvatar
        self.onSwitchBaby = onSwitchBaby
        self.onOpenAllCheckIns = onOpenAllCheckIns
        _model = State(wrappedValue: InsightsViewModel(
            store: store, babyID: babyID, volumeUnit: volumeUnit
        ))
    }

    public var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 10) {
                    ForEach(model.sections) { section in
                        InsightSectionCard(section: section)
                    }
                    GrowthSectionView(model: model.growth, store: store, babyID: babyID,
                                      onOpenAllCheckIns: onOpenAllCheckIns)
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 12)
            }
            .background(Palette.ground.ignoresSafeArea())
            // The title lived in a navigation bar, which reserved a large
            // bar's worth of empty space above it and then said "Insights" —
            // a word the tab bar was already saying. The header below carries
            // the baby's name instead, the one thing the bar never did.
            #if os(iOS)
            .toolbar(.hidden, for: .navigationBar)
            #endif
            .safeAreaInset(edge: .top, spacing: 0) { header }
        }
        .task { reload() }
        // Every figure on this screen is an average over entries. A write
        // anywhere — including the widget, via `observeRemoteChanges` — makes
        // all of them stale until this runs.
        .onChange(of: store.revision) { _, _ in reload() }
        // The unit is a preference and can change while this screen is alive.
        // Every volume on it is formatted from the view model, so a new unit
        // has to be handed over and the screen re-derived — re-rendering with
        // the old unit in the model would change nothing at all.
        .onChange(of: volumeUnit) { _, unit in
            model.volumeUnit = unit
            reload()
        }
        .onChange(of: range) { _, value in
            model.range = value
            reload()
        }
        // Returning to the app can also mean the day rolled over, which moves
        // every period's boundaries.
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { reload() }
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 8) {
            name
            Picker(L10n.tr("Range"), selection: $range) {
                ForEach(InsightRange.allCases) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .controlSize(.large)
            .accessibilityIdentifier("insights.range")
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        // Home's inset, so the switcher sits in the same place on every tab.
        .padding(.horizontal, 20)
        .padding(.vertical, 8)
        .background(.bar)
    }

    private var name: some View {
        BabySwitcherHeader(babyName: babyName, avatar: babyAvatar,
                           switcherID: "insights.babySwitcher", headingID: "insights.babyName",
                           onSwitchBaby: onSwitchBaby)
    }

    /// The one load path. A failure says so in the banner rather than leaving
    /// stale averages on screen as if they were current.
    private func reload() {
        do {
            try model.load()
            // Only a stale load banner is cleared. A save failure's banner must
            // survive a successful read: a parent who sees it vanish assumes
            // the entry went in and does not re-log it.
            if errorReporter.current == ErrorBannerCopy.loadFailed { errorReporter.clear() }
        } catch {
            errorReporter.report(
                ErrorBannerCopy.loadFailed,
                error: error,
                category: AppLog.store
            )
        }
    }
}
