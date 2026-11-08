import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public struct HistoryFocus: Equatable {
    public let id = UUID()
    /// An entry to open in its edit sheet (an Ask source).
    public let entryID: UUID?
    /// Trackers to show (Insights' All check-ins).
    public let filter: Set<Tracker>?

    public init(entryID: UUID) { self.entryID = entryID; self.filter = nil }
    public init(filter: Set<Tracker>) { self.entryID = nil; self.filter = filter }
}

/// What a tap on History asks the app to open. HistoryFeature presents no
/// editor itself: `RootView` maps each action to a LoggingFeature sheet, or
/// to Home's running-timer editor, as it does Home's dock (ruling R4).
public enum HistoryAction: Equatable {
    /// One entry, in the sheet it was logged with, in edit mode.
    case edit(EntryRef)
    /// A feed or sleep of several parts, in the session sheet.
    case session(SessionKind, rootID: UUID)
    /// A running feed or sleep, in the running-timer editor (ruling H-R5).
    case runningTimer(UUID)

    init(_ target: HistoryRowTarget) {
        switch target {
        case .entry(let ref): self = .edit(ref)
        case let .session(kind, rootID): self = .session(kind, rootID: rootID)
        case .runningTimer(let id): self = .runningTimer(id)
        }
    }
}

/// The days, newest first: one row per feed or sleep session or entry, start
/// over end on the left (mockup A2), under pinned day headers with icon
/// totals. A tap opens the row's sheet through `onAction`; a swipe deletes,
/// asking first for a whole session. A view-only caregiver reads the same
/// list with nothing to tap or swipe (ruling H-R6).
///
/// Accessibility identifiers: `history.babyName` (or `history.babySwitcher`
/// when the name switches babies), `history.export` (Export PDF; its sheet
/// documents its own identifiers), `history.filter`, `history.filter.clear`,
/// the filter sheet's `history.filter.<tracker>` and `history.filter.done`,
/// `history.day.<n>`, `history.row.<n>` (counted down the whole list from 0;
/// a running row's live duration is read as part of it, so it has no
/// queryable identifier of its own), `history.loadingMore` (the paging footer), `history.delete` (the swipe action), `history.confirmDelete` and
/// `history.keep` (a session's delete confirmation), `history.empty` and
/// `history.empty.clearFilter`.
public struct HistoryView: View {
    private let store: BabyStore
    private let volumeUnit: VolumeUnit
    /// Shown in the header: a two-baby household needs to tell whose list
    /// this is.
    private let babyName: String
    private let babyAvatar: BabyAvatar
    private let babyID: UUID
    private let focus: HistoryFocus?
    /// Called once a focus is applied, so the app can drop it: the tabs
    /// remount per baby, and a focus left standing would be applied again
    /// to the next baby's History.
    private let onFocusHandled: () -> Void
    private let onAction: (HistoryAction) -> Void
    /// The name is the baby switcher, as on Home, when the app offers one.
    private let onSwitchBaby: (() -> Void)?
    @State private var model: HistoryViewModel
    @State private var isShowingFilter = false
    @State private var isExporting = false
    /// The session row whose swipe is waiting on "Delete this feed?".
    @State private var pendingDelete: HistoryListRow?
    @State private var isConfirmingDelete = false
    @Environment(\.errorReporter) private var errorReporter
    @Environment(\.scenePhase) private var scenePhase

    /// Rows sit close, as in the mockup: the list's default insets around a
    /// 44-point row left a day of entries spread over two screens.
    private static let rowInsets = EdgeInsets(top: Spacing.xs, leading: Spacing.l, bottom: Spacing.xs, trailing: Spacing.l)

    public init(
        store: BabyStore,
        babyID: UUID,
        babyName: String = "",
        babyAvatar: BabyAvatar = .neutral,
        volumeUnit: VolumeUnit = .millilitres,
        focus: HistoryFocus? = nil,
        onFocusHandled: @escaping () -> Void = {},
        onSwitchBaby: (() -> Void)? = nil,
        onAction: @escaping (HistoryAction) -> Void
    ) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
        self.babyName = babyName
        self.babyAvatar = babyAvatar
        self.focus = focus
        self.onFocusHandled = onFocusHandled
        self.onSwitchBaby = onSwitchBaby
        self.onAction = onAction
        _model = State(wrappedValue: HistoryViewModel(
            store: store, babyID: babyID, volumeUnit: volumeUnit
        ))
    }

    public var body: some View {
        NavigationStack {
            ScreenScaffold(
                header: .baby(name: babyName, avatar: babyAvatar, switcherID: "history.babySwitcher",
                              headingID: "history.babyName", onSwitch: onSwitchBaby),
                scrolls: false,
                trailing: {
                    exportButton
                    filterButton
                },
                content: {
                    if model.isFiltered {
                        showingChip
                    }
                    list
                }
            )
                .navigationTitle(HistoryCopy.title)
                #if os(iOS)
                // The header is drawn by the scaffold, so the bar would leave
                // a tall empty band above it.
                .toolbar(.hidden, for: .navigationBar)
                #endif
                .overlay {
                    // While older days may still hold something, the footer
                    // keeps paging rather than calling the list empty.
                    if let empty = model.emptyState, empty == .noData || !model.hasMore { emptyView(empty) }
                }
        }
        .fittedSheet(isPresented: $isShowingFilter) {
            HistoryFilterSheet(filter: model.filter, toggle: toggle) { isShowingFilter = false }
        }
        .fittedSheet(isPresented: $isExporting) {
            HistoryExportSheet(
                model: HistoryExportViewModel(store: store, babyID: babyID, babyName: babyName, volumeUnit: model.volumeUnit)
            ) { isExporting = false }
        }
        .confirmDelete(
            isPresented: $isConfirmingDelete,
            title: pendingDelete.map(HistoryCopy.deleteConfirmTitle) ?? "",
            deleteTitle: HistoryCopy.delete,
            keepTitle: HistoryCopy.keep,
            identifierPrefix: "history"
        ) {
            if let row = pendingDelete { deleteConfirmed(row) }
        }
        .task { reload(); openFocusedEntry() }
        .onChange(of: focus?.id) { _, _ in openFocusedEntry() }
        // A write anywhere in the app — a sheet, Home's quick log, the other
        // phone through sync — bumps the revision, and the list follows.
        .onChange(of: store.revision) { _, _ in reload() }
        // The unit is a preference and can change while this screen is alive.
        // Every volume on it is formatted by the view model, so a new unit
        // has to be handed over and the screen re-derived.
        .onChange(of: volumeUnit) { _, unit in
            model.volumeUnit = unit
            reload()
        }
        // A write from another process — the widget, an App Intent — does
        // not bump `revision`. Coming back to the app is the moment to re-read.
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { reload() }
        }
    }

    // MARK: - List

    private var list: some View {
        let offsets = rowOffsets
        return List {
            ForEach(Array(model.days.enumerated()), id: \.element.id) { dayIndex, day in
                Section {
                    ForEach(Array(day.rows.enumerated()), id: \.element.id) { rowIndex, row in
                        rowView(row, index: offsets[dayIndex] + rowIndex)
                    }
                } header: {
                    HistoryDayHeader(day: day, volumeUnit: model.volumeUnit)
                        .accessibilityIdentifier("history.day.\(dayIndex)")
                }
            }
            if model.hasMore {
                pageLoader
            }
        }
        // Each day is a card on the scaffold's ground; the list stays a
        // `List` so the swipe actions keep working.
        .cardListStyle()
    }

    /// The end of the list while older days remain (ruling H-R23). Reaching
    /// it loads the next page; keyed on the window, so a page that brought
    /// no rows (a quiet fortnight) loads the next while this is still in
    /// view, until rows push it off screen or there is nothing older.
    private var pageLoader: some View {
        ProgressView()
            .frame(maxWidth: .infinity, minHeight: 44)
            .listRowSeparator(.hidden)
            .listRowBackground(Color.clear)
            .accessibilityIdentifier("history.loadingMore")
            .task(id: model.dayCount) { loadMore() }
    }

    /// Where each day's rows start in the whole list's count, so a row's
    /// identifier says where it is on screen.
    private var rowOffsets: [Int] {
        var offsets: [Int] = []
        var next = 0
        for day in model.days {
            offsets.append(next)
            next += day.rows.count
        }
        return offsets
    }

    @ViewBuilder
    private func rowView(_ row: HistoryListRow, index: Int) -> some View {
        let content = HistoryListRowView(row: row, index: index)
        if model.canEdit {
            Button { onAction(HistoryAction(row.target)) } label: {
                // Load-bearing: a plain-styled button's hit region is
                // otherwise only its drawn content.
                content.contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityIdentifier("history.row.\(index)")
            .listRowSeparator(.hidden)
            .listRowBackground(Surface.card)
            .listRowInsets(Self.rowInsets)
            #if os(iOS)
            .swipeActions(edge: .trailing, allowsFullSwipe: !row.deleteConfirmation) {
                // A running timer is stopped or discarded in its own editor.
                if !row.isRunning { deleteAction(row) }
            }
            #endif
        } else {
            content
                .accessibilityIdentifier("history.row.\(index)")
                .listRowSeparator(.hidden)
                .listRowBackground(Surface.card)
                .listRowInsets(Self.rowInsets)
        }
    }

    @ViewBuilder
    private func deleteAction(_ row: HistoryListRow) -> some View {
        if row.deleteConfirmation {
            // No destructive role: that role removes the row at once, before
            // the confirmation has been answered.
            Button { pendingDelete = row; isConfirmingDelete = true } label: {
                Label(HistoryCopy.delete, systemImage: "trash")
            }
            .tint(Palette.destructive)
            .accessibilityIdentifier("history.delete")
        } else {
            Button(role: .destructive) { delete(row) } label: {
                Label(HistoryCopy.delete, systemImage: "trash")
            }
            .accessibilityIdentifier("history.delete")
        }
    }

    // MARK: - Header

    // MARK: - Header

    private var exportButton: some View {
        CircleIconButton(systemImage: "square.and.arrow.up", label: HistoryCopy.exportPDF,
                         identifier: "history.export") { isExporting = true }
    }

    private var filterButton: some View {
        CircleIconButton(systemImage: "line.3.horizontal.decrease", label: HistoryCopy.filter,
                         identifier: "history.filter", showsBadge: model.isFiltered) {
            isShowingFilter = true
        }
        .accessibilityValue(model.isFiltered ? HistoryCopy.showing(model.filter) : "")
    }

    /// One tap clears the filter.
    private var showingChip: some View {
        HStack {
            Chip(HistoryCopy.showing(model.filter), onRemove: clearFilter,
                 removeLabel: HistoryCopy.clearFilter, removeIdentifier: "history.filter.clear")
            Spacer(minLength: 0)
        }
        .padding(.horizontal, Spacing.l)
    }

    // MARK: - Empty

    @ViewBuilder
    private func emptyView(_ empty: HistoryEmptyState) -> some View {
        switch empty {
        case .noData:
            // One line with the icon: the list will fill in as things are
            // logged, so there is nothing to explain.
            EmptyState(systemImage: "list.bullet", title: HistoryCopy.nothingLogged, message: nil)
                .accessibilityIdentifier("history.empty")
        case .nothingMatches:
            VStack(spacing: Spacing.s) {
                EmptyState(systemImage: "line.3.horizontal.decrease", title: HistoryCopy.nothingMatches, message: nil)
                    .accessibilityIdentifier("history.empty")
                Button(HistoryCopy.clearFilter, action: clearFilter)
                    .buttonStyle(.secondary)
                    .padding(.horizontal, Spacing.l)
                    .accessibilityIdentifier("history.empty.clearFilter")
            }
        case .nothingRecent(let dayCount):
            EmptyState(
                systemImage: "list.bullet",
                title: HistoryCopy.nothingRecentTitle(dayCount),
                message: HistoryCopy.nothingRecentMessage(dayCount)
            )
            .accessibilityIdentifier("history.empty")
        }
    }

    // MARK: - Actions

    /// The one load path. A failure says so in the banner rather than leaving
    /// the last-loaded list on screen with no explanation.
    private func reload() {
        do {
            try model.reload()
            // Only a stale load banner is cleared. A save failure's banner must
            // survive a successful read: a parent who sees it vanish assumes
            // the entry went in and does not re-log it.
            if errorReporter.current == ErrorBannerCopy.loadFailed { errorReporter.clear() }
        } catch {
            errorReporter.report(ErrorBannerCopy.loadFailed, error: error, category: AppLog.store)
        }
    }

    private func loadMore() {
        do {
            try model.loadMore()
        } catch {
            errorReporter.report(ErrorBannerCopy.loadFailed, error: error, category: AppLog.store)
        }
    }

    private func toggle(_ tracker: Tracker) {
        if model.filter.contains(tracker) {
            // The last kind stays on: an empty filter shows nothing at all,
            // which reads as lost data rather than a choice.
            guard model.filter.count > 1 else { return }
            model.filter.remove(tracker)
        } else {
            model.filter.insert(tracker)
        }
        reload()
    }

    private func clearFilter() {
        model.filter = Set(HistoryViewModel.trackers)
        reload()
    }

    /// An Ask source opens its entry's edit sheet with the filter cleared, so
    /// its row is behind the sheet (ruling H-R9). Insights' All check-ins
    /// sets the Growth filter. Either way the focus is then spent, and the
    /// app is told so it does not outlive this History.
    private func openFocusedEntry() {
        guard let focus = model.consume(focus) else { return }
        reload()
        onFocusHandled()
        guard let entryID = focus.entryID, model.canEdit else { return }
        do {
            if let ref = try model.entryRef(for: entryID) {
                onAction(.edit(ref))
            }
        } catch {
            errorReporter.report(ErrorBannerCopy.loadFailed, error: error, category: AppLog.store)
        }
    }

    /// A confirmed session delete: nothing happens if the row changed under
    /// the dialog (the list already shows how).
    private func deleteConfirmed(_ row: HistoryListRow) {
        do {
            try model.deleteConfirmed(row)
            errorReporter.clear()
        } catch {
            errorReporter.report(ErrorBannerCopy.deleteFailed(what: ErrorSubject.entry),
                                 error: error, category: AppLog.store)
        }
    }

    private func delete(_ row: HistoryListRow) {
        do {
            try model.delete(row)
            errorReporter.clear()
        } catch {
            // The row springs back on its own; the banner says why it did.
            errorReporter.report(ErrorBannerCopy.deleteFailed(what: ErrorSubject.entry),
                                 error: error, category: AppLog.store)
        }
    }
}

public enum HistoryCopy {
    public static let title = L10n.tr("History")
    static let pumping = L10n.tr("Pumping…")
    /// A finished session with no amount. A dash, not a word: nothing to translate.
    static let noAmount = "—"
    static let filter = L10n.tr("Filter")
    static let clearFilter = L10n.tr("Clear filter")
    static let delete = L10n.tr("Delete")
    static let keep = L10n.tr("Keep it")
    static let nothingLogged = L10n.tr("Nothing logged yet")
    static let nothingMatches = L10n.tr("Nothing matches")
    static let show = L10n.tr("Show")
    static let done = L10n.tr("Done")
    static let cancel = L10n.tr("Cancel")
    static let exportPDF = L10n.tr("Export PDF")
    static let columnTime = L10n.tr("Time")
    static let columnType = L10n.tr("Type")
    static let columnDetails = L10n.tr("Details")
    static let columnNote = L10n.tr("Note")

    static func generated(_ when: String) -> String { L10n.fmt("Generated %@", when) }
    static func page(_ number: Int, of total: Int) -> String { L10n.fmt("Page %lld of %lld", number, total) }

    static func nothingRecentTitle(_ dayCount: Int) -> String {
        L10n.plural("Nothing in the last %lld days", dayCount)
    }

    static func nothingRecentMessage(_ dayCount: Int) -> String {
        L10n.plural("Older entries are still saved — this screen shows the last %lld days.", dayCount)
    }

    /// The filter sheet's name for a tracker: the plural a list of them
    /// reads as ("Feeds", "Diapers").
    static func filterName(_ tracker: Tracker) -> String {
        switch tracker {
        case .feed, .solids: L10n.tr("Feeds")
        case .sleep: L10n.tr("Sleep")
        case .diaper: L10n.tr("Diapers")
        case .supplement: L10n.tr("Meds")
        case .growth: L10n.tr("Growth")
        case .vaccine: L10n.tr("Vaccines")
        case .pump: L10n.tr("Pumping")
        }
    }

    /// "Showing: Feeds", "Showing: Feeds and Sleep", in filter-sheet order.
    @MainActor static func showing(_ filter: Set<Tracker>) -> String {
        let names = HistoryViewModel.trackers.filter(filter.contains).map(filterName)
        return L10n.fmt("Showing: %@", names.formatted(.list(type: .and)))
    }

    static func deleteConfirmTitle(_ row: HistoryListRow) -> String {
        row.tracker == .sleep ? L10n.tr("Delete this sleep?") : L10n.tr("Delete this feed?")
    }
}

// MARK: - Day header

/// "Today", then the day's icon totals: feeds, the asleep total, bottle
/// volume, diapers. A tracker with nothing that day, or filtered out, shows
/// no total.
struct HistoryDayHeader: View {
    let day: HistoryDay
    let volumeUnit: VolumeUnit
    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        Group {
            if typeSize.isAccessibilitySize {
                VStack(alignment: .leading, spacing: Spacing.xs) {
                    title
                    totals
                }
            } else {
                HStack(alignment: .firstTextBaseline, spacing: Spacing.m) {
                    title
                    Spacer(minLength: 0)
                    totals
                }
            }
        }
        .padding(.vertical, Spacing.xs)
        .textCase(nil)
        .accessibilityElement(children: .ignore)
        .accessibilityAddTraits(.isHeader)
        .accessibilityLabel(([day.title] + spokenTotals).joined(separator: ", "))
    }

    private var title: some View {
        Text(day.title)
            .font(.headline)
            .foregroundStyle(Palette.primaryText)
    }

    @ViewBuilder private var totals: some View {
        if typeSize.isAccessibilitySize {
            ChipFlow(spacing: Spacing.m) { totalItems }
        } else {
            HStack(spacing: Spacing.m) { totalItems }
        }
    }

    @ViewBuilder private var totalItems: some View {
        Group {
            if day.totals.feeds > 0 {
                total(Tracker.feed.icon, Tracker.feed.color, "\(day.totals.feeds)")
            }
            if !day.totals.sleep.isEmpty {
                total(Tracker.sleep.icon, Tracker.sleep.color, day.totals.sleep)
            }
            if day.totals.bottleML > 0 {
                total(.bottle, Tracker.feed.color, Volume(millilitres: day.totals.bottleML).formatted(as: volumeUnit))
            }
            if day.totals.diapers > 0 {
                total(Tracker.diaper.icon, Tracker.diaper.color, "\(day.totals.diapers)")
            }
        }
    }

    private func total(_ icon: BabyIcon, _ color: Color, _ value: String) -> some View {
        HStack(spacing: 3) {
            BabyIconView(icon, size: 14)
                .foregroundStyle(color)
            Text(value)
                .font(.footnote.weight(.bold))
                .monospacedDigit()
                .lineLimit(1)
                .fixedSize()
                .foregroundStyle(Palette.primaryText)
        }
    }

    private var spokenTotals: [String] { day.totals.phrases(volumeUnit: volumeUnit) }
}

// MARK: - Row

/// Start over end, the icon, then the parts as chips or the entry's text
/// (mockup A2). A running row adds its live duration, ticking from the
/// moment its current part began.
struct HistoryListRowView: View {
    let row: HistoryListRow
    let index: Int
    @ScaledMetric(relativeTo: .subheadline) private var timeWidth = 48

    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        Group {
            if typeSize.isAccessibilitySize {
                // Time and glyph above, the content under them at the full
                // width: beside them it would break words mid-word.
                VStack(alignment: .leading, spacing: Spacing.xs) {
                    HStack(alignment: .firstTextBaseline, spacing: Spacing.m) {
                        times(stacked: false)
                        glyph
                    }
                    details
                }
            } else {
                HStack(alignment: .center, spacing: Spacing.m) {
                    times(stacked: true)
                    glyph
                    details
                }
            }
        }
        .frame(minHeight: 40)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(label)
    }

    @ViewBuilder
    private func times(stacked: Bool) -> some View {
        let layout = stacked ? AnyLayout(VStackLayout(alignment: .leading, spacing: 1))
                             : AnyLayout(HStackLayout(alignment: .firstTextBaseline, spacing: Spacing.s))
        layout {
            Text(row.start)
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(Palette.primaryText)
            if let end = row.end {
                Text(stacked ? end : "–\(end)")
                    .font(.caption)
                    .foregroundStyle(Palette.secondaryText)
            }
        }
        .monospacedDigit()
        .lineLimit(1)
        .fixedSize()
        .frame(minWidth: stacked ? timeWidth : nil, alignment: .leading)
    }

    private var glyph: some View {
        TrackerIcon(row.tracker)
            .foregroundStyle(row.tracker.color)
            .accessibilityHidden(true)
    }

    private var details: some View {
        ChipFlow(spacing: Spacing.s) {
            if let text = row.text {
                Text(text)
                    .font(.body)
                    .foregroundStyle(Palette.primaryText)
            }
            ForEach(row.chips) { chip in
                Chip(chip.text, icon: chip.icon, tint: row.tracker.color, size: .small, monospacedDigits: true)
            }
            if let since = row.runningSince {
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    Text(ElapsedFormatter.precise(Int(context.date.timeIntervalSince(since))))
                        .font(.subheadline.weight(.semibold))
                        .monospacedDigit()
                        .foregroundStyle(row.tracker.color)
                }
                .accessibilityIdentifier("history.row.\(index).running")
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// "Diaper, Wet, at 13:20", "Feed, L 12m, R 15m, at 10:23–11:48". A
    /// running row with no finished part yet has nothing to list, and says
    /// "Sleep, at 15:41–now" rather than leave an empty item.
    private var label: String {
        let time = row.end.map { "\(row.start)–\($0)" } ?? row.start
        return summary.isEmpty
            ? L10n.fmt("%@, at %@", row.tracker.title, time)
            : L10n.fmt("%@, %@, at %@", row.tracker.title, summary, time)
    }

    private var summary: String {
        ([row.text].compactMap { $0 } + row.chips.map(\.text)).joined(separator: ", ")
    }
}

/// Lays its children out left to right, wrapping to a new line when the
/// next one would not fit — a row's chips at the largest text sizes.
private struct ChipFlow: Layout {
    var spacing: CGFloat

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let lines = arrange(subviews, width: proposal.width ?? .infinity)
        let width = lines.map(\.width).max() ?? 0
        let height = lines.map(\.height).reduce(0, +) + spacing * CGFloat(max(lines.count - 1, 0))
        return CGSize(width: width, height: height)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var y = bounds.minY
        for line in arrange(subviews, width: bounds.width) {
            var x = bounds.minX
            for (index, size) in zip(line.indices, line.sizes) {
                subviews[index].place(at: CGPoint(x: x, y: y + (line.height - size.height) / 2),
                                      proposal: ProposedViewSize(size))
                x += size.width + spacing
            }
            y += line.height + spacing
        }
    }

    private struct Line {
        var indices: [Int] = []
        var sizes: [CGSize] = []
        var width: CGFloat = 0
        var height: CGFloat = 0
    }

    private func arrange(_ subviews: Subviews, width: CGFloat) -> [Line] {
        var lines: [Line] = []
        var line = Line()
        for index in subviews.indices {
            // A child wider than the line (long foods at XXXL) gets the
            // line's width and wraps its own text.
            var size = subviews[index].sizeThatFits(.unspecified)
            if size.width > width {
                size = subviews[index].sizeThatFits(ProposedViewSize(width: width, height: nil))
            }
            if !line.indices.isEmpty, line.width + spacing + size.width > width {
                lines.append(line)
                line = Line()
            }
            line.width += (line.indices.isEmpty ? 0 : spacing) + size.width
            line.height = max(line.height, size.height)
            line.indices.append(index)
            line.sizes.append(size)
        }
        if !line.indices.isEmpty { lines.append(line) }
        return lines
    }
}

// MARK: - Filter sheet

/// One icon toggle per tracker, all on by default. The last one on stays on.
struct HistoryFilterSheet: View {
    let filter: Set<Tracker>
    let toggle: (Tracker) -> Void
    let onDone: () -> Void
    @Environment(\.dynamicTypeSize) private var typeSize

    /// Presented with `fittedSheet`: as tall as this, so the title, the close
    /// button and Done are drawn here. The toggles apply as they are tapped
    /// (`toggle` reloads the list), so Done only closes the sheet.
    var body: some View {
        SheetScaffold(
            HistoryCopy.show,
            closeTitle: HistoryCopy.cancel,
            onClose: onDone,
            primary: SheetAction(HistoryCopy.done, identifier: "history.filter.done", action: onDone)
        ) {
            if typeSize.isAccessibilitySize {
                VStack(alignment: .leading, spacing: Spacing.s) {
                    ForEach(HistoryViewModel.trackers, id: \.self) { toggleButton($0) }
                }
            } else {
                // Wraps onto more lines rather than shrinking a chip.
                ChipFlow(spacing: Spacing.s) {
                    ForEach(HistoryViewModel.trackers, id: \.self) { toggleButton($0) }
                }
            }
        }
    }

    private func toggleButton(_ tracker: Tracker) -> some View {
        let isOn = filter.contains(tracker)
        let isLast = isOn && filter.count == 1
        return Button { toggle(tracker) } label: {
            HStack(spacing: Spacing.s) {
                BabyIconView(tracker.icon, size: 18)
                    .accessibilityHidden(true)
                Text(HistoryCopy.filterName(tracker))
            }
            .frame(maxWidth: typeSize.isAccessibilitySize ? .infinity : nil, alignment: .leading)
        }
        .buttonStyle(.selectableChip(tracker.color, isSelected: isOn))
        .disabled(isLast)
        .accessibilityIdentifier("history.filter.\(tracker.rawValue)")
    }
}
