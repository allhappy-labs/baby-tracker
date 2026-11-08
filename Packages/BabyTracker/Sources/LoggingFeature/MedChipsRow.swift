import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// The saved meds as horizontally scrolling chips, "+" always last. A tap
/// logs that med now and closes the sheet.
struct MedChipsRow: View {
    let model: MoreSheetModel
    /// "more" in the ··· sheet, "meds" in the Meds panel.
    let idPrefix: String
    let onAdd: () -> Void
    let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: Spacing.s) {
                ForEach(Array(model.meds.enumerated()), id: \.offset) { index, name in
                    medChip(name, index: index)
                }
                addMedChip
            }
        }
        // Chips scroll to the sheet's edge, past the content inset.
        .scrollClipDisabled()
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
            HStack(spacing: Spacing.xs) {
                BabyIconView(.meds, size: 17)
                Text(name).lineLimit(1)
            }
        }
        .buttonStyle(.selectableChip(Palette.meds, isSelected: false))
        .accessibilityIdentifier("\(idPrefix).med.\(index)")
    }

    private var addMedChip: some View {
        Button(action: onAdd) {
            BabyIconView(.plus, size: 17)
                .frame(minWidth: 20)
        }
        .buttonStyle(.selectableChip(Palette.secondaryText, isSelected: false))
        .accessibilityLabel(SupplementSheetCopy.addName)
        .accessibilityIdentifier("\(idPrefix).addMed")
    }
}
