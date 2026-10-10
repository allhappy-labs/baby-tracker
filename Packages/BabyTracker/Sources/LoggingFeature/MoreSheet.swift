import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public enum MoreSheetCopy {
    public static let title = LoggingL10n.text("More")
    public static let cancel = LoggingL10n.text("Cancel")
    public static let pastEntry = LoggingL10n.text("Past entry")
}

/// Everything else the dock doesn't have room for: Solids, Growth and Past
/// entry up top, a horizontally scrolling row of saved Meds below with "+"
/// always last.
@MainActor @Observable
public final class MoreSheetModel {
    private let store: BabyStore
    private let babyID: UUID

    /// Saved med names, most recent first.
    public private(set) var meds: [String] = []

    public init(store: BabyStore, babyID: UUID) {
        self.store = store
        self.babyID = babyID
        self.meds = (try? store.supplementNames(for: babyID)) ?? []
    }

    /// Logs `name` right now and refreshes the row, so a second tap before
    /// the sheet dismisses sees the same order the store would report.
    public func logMed(_ name: String) throws {
        try store.logSupplement(name: name, for: babyID)
        meds = (try? store.supplementNames(for: babyID)) ?? meds
    }
}

public struct MoreSheet: View {
    @State private var model: MoreSheetModel
    private let onSwitchTo: (LogSheet) -> Void
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter
    /// Three tiles share a row at the ordinary sizes; at the accessibility
    /// sizes a third of the sheet split "Solids" mid-word, so they stack.
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(
        store: BabyStore,
        babyID: UUID,
        onSwitchTo: @escaping (LogSheet) -> Void,
        onDone: @escaping () -> Void
    ) {
        _model = State(initialValue: MoreSheetModel(store: store, babyID: babyID))
        self.onSwitchTo = onSwitchTo
        self.onDone = onDone
    }

    /// Presented with `fittedSheet`: the sheet is as tall as this, so the
    /// title and Cancel are drawn here rather than in a navigation bar.
    public var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                SheetHeader(MoreSheetCopy.title, cancelTitle: MoreSheetCopy.cancel, onCancel: onDone)
                    .padding(.horizontal, -20)

                if typeSize.isAccessibilitySize {
                    VStack(spacing: 10) { tiles }
                } else {
                    HStack(spacing: 10) { tiles }
                }

                VStack(alignment: .leading, spacing: 8) {
                    Text(Tracker.supplement.title)
                        .font(.footnote)
                        .foregroundStyle(Palette.secondaryText)
                    ScrollView(.horizontal, showsIndicators: false) {
                        HStack(spacing: 8) {
                            ForEach(Array(model.meds.enumerated()), id: \.offset) { index, name in
                                medChip(name, index: index)
                            }
                            addMedChip
                        }
                    }
                }
            }
            .padding(.horizontal, 20)
            .padding(.bottom, 12)
        }
        // Past the screen (the largest text sizes) it scrolls rather than
        // clips; otherwise it does not bounce.
        .scrollBounceBehavior(.basedOnSize)
    }

    @ViewBuilder
    private var tiles: some View {
        tile(title: Tracker.solids.title, icon: Tracker.solids.icon, color: Tracker.solids.color, id: "more.solids") {
            onSwitchTo(.solids)
        }
        tile(title: Tracker.growth.title, icon: Tracker.growth.icon, color: Tracker.growth.color, id: "more.growth") {
            onSwitchTo(.measurement)
        }
        tile(title: MoreSheetCopy.pastEntry, icon: .clock, color: Palette.secondaryText, id: "more.pastEntry") {
            onSwitchTo(.pastEntry)
        }
    }

    private func tile(title: String, icon: BabyIcon, color: Color, id: String, action: @escaping () -> Void) -> some View {
        let glyph = BabyIconView(icon, size: 22).foregroundStyle(color)
        // Never shrunk to fit and never cut: the words wrap.
        let label = Text(title)
            .font(.caption.weight(.semibold))
            .fixedSize(horizontal: false, vertical: true)
        return Button(action: action) {
            Group {
                if typeSize.isAccessibilitySize {
                    HStack(spacing: 10) { glyph; label; Spacer(minLength: 0) }
                        .padding(.horizontal, 6)
                } else {
                    VStack(spacing: 6) { glyph; label.multilineTextAlignment(.center) }
                }
            }
            .frame(maxWidth: .infinity, minHeight: 76)
        }
        .buttonStyle(.tracker(color))
        .accessibilityIdentifier(id)
    }

    private func medChip(_ name: String, index: Int) -> some View {
        Button {
            do {
                try model.logMed(name)
                errorReporter.clear()
                onDone()
            } catch {
                errorReporter.report(ErrorBannerCopy.saveFailed(what: ErrorSubject.supplement), error: error, category: AppLog.store)
            }
        } label: {
            HStack(spacing: 6) {
                BabyIconView(.meds, size: 17)
                Text(name).lineLimit(1)
            }
            .padding(.horizontal, 14)
            .frame(minHeight: 44)
        }
        .buttonStyle(.tracker(Palette.meds))
        .accessibilityIdentifier("more.med.\(index)")
    }

    private var addMedChip: some View {
        Button {
            onSwitchTo(.supplement)
        } label: {
            BabyIconView(.plus, size: 17)
                .frame(width: 44, height: 44)
        }
        .buttonStyle(.tracker(Palette.secondaryText))
        .accessibilityLabel(SupplementSheetCopy.addName)
        .accessibilityIdentifier("more.addMed")
    }
}
