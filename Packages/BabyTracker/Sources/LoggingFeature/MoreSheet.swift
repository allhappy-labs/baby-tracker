import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public enum MoreSheetCopy {
    public static let title = LoggingL10n.text("More")
    public static let cancel = LoggingL10n.text("Cancel")
    public static let pastEntry = LoggingL10n.text("Log a past entry")
    public static let editHomeButtons = LoggingL10n.text("Edit Home buttons")
    public static func breast(_ side: BreastSide) -> String {
        LoggingL10n.text(side == .left ? "Left breast" : "Right breast")
    }
}

/// Everything the dock has no room for, by the baby's order, and the saved
/// meds while Meds itself is not on the dock.
@MainActor @Observable
public final class MoreSheetModel {
    private let store: BabyStore
    private let babyID: UUID

    /// Saved med names, most recent first.
    public private(set) var meds: [String] = []
    /// The actions offered as tiles, in order. Never Meds — its chips are
    /// the row below — and never an action that conflicts with a running
    /// timer.
    public let tiles: [HomeActionKind]
    public let showsMeds: Bool

    public init(store: BabyStore, babyID: UUID, order: [HomeActionKind] = HomeActionKind.defaultOrder) {
        self.store = store
        self.babyID = babyID
        self.meds = (try? store.supplementNames(for: babyID)) ?? []
        let timers = (try? store.activeTimers(for: babyID)) ?? []
        let overflow = HomeLayout(order: order).overflow(feedRunning: timers.contains { $0.kind == .feed },
                                                         sleepRunning: timers.contains { $0.kind == .sleep })
        self.tiles = overflow.filter { $0 != .meds }
        self.showsMeds = overflow.contains(.meds)
    }

    /// Logs `name` right now and refreshes the row, so a second tap before
    /// the sheet dismisses sees the same order the store would report.
    public func logMed(_ name: String) throws {
        try store.logSupplement(name: name, for: babyID)
        meds = (try? store.supplementNames(for: babyID)) ?? meds
    }

    /// Home's own start, for a baby whose order keeps Breast off the dock.
    public func startBreast(_ side: BreastSide) async throws {
        let runningID = try store.activeTimer(kind: .feed, for: babyID)?.id
        let timer = try store.startPart(.breast(side), for: babyID)
        guard timer.id != runningID else { return }
        await LiveActivityController.adoptOrStart(for: timer, in: store)
    }

    public func startSleep() async throws {
        guard try store.activeTimer(kind: .sleep, for: babyID) == nil else { return }
        let timer = try store.startSleep(for: babyID)
        await LiveActivityController.adoptOrStart(for: timer, in: store)
    }
}

public struct MoreSheet: View {
    @State private var model: MoreSheetModel
    private let onSwitchTo: (LogSheet) -> Void
    private let onEditHomeButtons: (() -> Void)?
    private let onDone: () -> Void
    @State private var isChoosingBreast = false
    @Environment(\.errorReporter) private var errorReporter
    /// Four icons share a row at the ordinary sizes, as on Home; at the
    /// accessibility sizes a quarter of the sheet would split the words, so
    /// they stack as rows.
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(
        store: BabyStore,
        babyID: UUID,
        order: [HomeActionKind] = HomeActionKind.defaultOrder,
        onSwitchTo: @escaping (LogSheet) -> Void,
        onEditHomeButtons: (() -> Void)? = nil,
        onDone: @escaping () -> Void
    ) {
        _model = State(initialValue: MoreSheetModel(store: store, babyID: babyID, order: order))
        self.onSwitchTo = onSwitchTo
        self.onEditHomeButtons = onEditHomeButtons
        self.onDone = onDone
    }

    private var columns: [GridItem] {
        Array(repeating: GridItem(.flexible(), spacing: Spacing.s, alignment: .top),
              count: typeSize.isAccessibilitySize ? 1 : 4)
    }

    /// Presented with `fittedSheet`: the sheet is as tall as this, so the
    /// title and close button are drawn here rather than in a navigation bar.
    public var body: some View {
        SheetScaffold(MoreSheetCopy.title, closeTitle: MoreSheetCopy.cancel, onClose: onDone) {
            LazyVGrid(columns: columns, spacing: Spacing.s) { tiles }
                .confirmationDialog(HomeActionKind.breast.title, isPresented: $isChoosingBreast) {
                    Button(MoreSheetCopy.breast(.left)) { start(ErrorSubject.feed) { try await model.startBreast(.left) } }
                    Button(MoreSheetCopy.breast(.right)) { start(ErrorSubject.feed) { try await model.startBreast(.right) } }
                }

            if model.showsMeds {
                VStack(alignment: .leading, spacing: Spacing.s) {
                    Text(Tracker.supplement.title)
                        .textRole(.sectionLabel)
                        .padding(.horizontal, Spacing.l)
                    MedChipsRow(model: model, idPrefix: "more", onAdd: { onSwitchTo(.supplement) }, onDone: onDone)
                }
            }

            // Tools, not trackers: plain rows under them.
            CardSection {
                toolRow(MoreSheetCopy.pastEntry, glyph: BabyIconView(.clock, size: 18), id: "more.pastEntry") {
                    onSwitchTo(.pastEntry)
                }
                if let onEditHomeButtons {
                    toolRow(MoreSheetCopy.editHomeButtons, glyph: Image(systemName: "house").font(.system(size: 16)),
                            id: "more.editHomeButtons", action: onEditHomeButtons)
                }
            }
        }
    }

    private var tiles: some View {
        ForEach(model.tiles, id: \.self) { kind in
            tile(title: kind.title, icon: kind.icon, color: kind.color, id: "more.\(kind.rawValue)") { run(kind) }
        }
    }

    private func run(_ kind: HomeActionKind) {
        switch kind {
        case .breast: isChoosingBreast = true
        case .sleep: start(ErrorSubject.sleep) { try await model.startSleep() }
        case .bottle: onSwitchTo(.bottle)
        case .diaper: onSwitchTo(.diaper)
        case .solids: onSwitchTo(.solids)
        case .growth: onSwitchTo(.measurement)
        case .vaccine: onSwitchTo(.immunization)
        case .pump: onSwitchTo(.pump)
        // Never a tile: its chips are the row below.
        case .meds: onSwitchTo(.meds)
        }
    }

    /// Starts a timer and closes the sheet. A failure says so and leaves the
    /// sheet up.
    private func start(_ subject: String, _ work: @escaping () async throws -> Void) {
        Task {
            do {
                try await work()
                errorReporter.clear()
                onDone()
            } catch {
                errorReporter.report(ErrorBannerCopy.saveFailed(what: subject), error: error, category: AppLog.store)
            }
        }
    }

    /// A tinted tile with the tracker's icon and its name under it, as on
    /// Home; at the accessibility sizes a row with the icon on the left.
    private func tile(title: String, icon: BabyIcon, color: Color, id: String, action: @escaping () -> Void) -> some View {
        let glyph = BabyIconView(icon, size: 26)
        // Never shrunk to fit and never cut: the words wrap.
        let label = Text(title)
            .font(.caption.weight(.semibold))
            .fixedSize(horizontal: false, vertical: true)
        return Button(action: action) {
            if typeSize.isAccessibilitySize {
                HStack(spacing: Spacing.m) { glyph; label; Spacer(minLength: 0) }
            } else {
                VStack(spacing: Spacing.xs) { glyph; label.multilineTextAlignment(.center) }
            }
        }
        .buttonStyle(.tile(color, size: .large))
        .accessibilityIdentifier(id)
    }

    /// A tool under the trackers: a row with its glyph, name and a chevron.
    private func toolRow(_ title: String, glyph: some View, id: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            ListRow(title, accessory: .disclosure, leading: {
                glyph
                    .foregroundStyle(Palette.secondaryText)
                    .frame(width: 32, height: 32)
                    .accessibilityHidden(true)
            }, trailing: { EmptyView() })
            .fixedSize(horizontal: false, vertical: true)
        }
        .buttonStyle(.plain)
        .accessibilityIdentifier(id)
    }
}
