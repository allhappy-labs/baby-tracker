import SwiftUI
import BabyCore
import BabyUI

/// A bottle's or a pump's amount, the same on every screen that sets one: − and + either
/// side of the number, which can also be typed, and the four amounts below.
/// A tap on an amount dials it in; the screen's own button writes.
struct BottleAmountSection: View {
    @Binding var volumeML: Double
    let unit: VolumeUnit
    let chips: [Double]
    let label: (Double) -> String
    let step: (Int) -> Void
    /// `partEdit`, `bottle`, `pump` or `pumpAmount`: the screen's own prefix
    /// for every control.
    let identifierPrefix: String
    /// The chips' colour: the feed's, or the pump's on the pump screens.
    var tint: Color = Palette.feed
    /// What VoiceOver calls the number: Volume for a bottle, Amount for a pump.
    var volumeLabel: String = BottleSheetCopy.volumeLabel
    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        CardSection {
            if typeSize.isAccessibilitySize {
                volume
                chipsGrid
                HStack(spacing: Spacing.s) {
                    minusButton.frame(maxWidth: .infinity)
                    plusButton.frame(maxWidth: .infinity)
                }
            } else {
                // − and + flank the amount rather than taking a row of their
                // own, so a bottle fits the screen without scrolling.
                HStack(spacing: Spacing.s) {
                    minusButton.frame(width: 64)
                    volume
                    plusButton.frame(width: 64)
                }
                chipsGrid
            }
        }
    }

    private var volume: some View {
        BottleVolumeField(volumeML: $volumeML, unit: unit, label: volumeLabel, style: .display)
            .frame(maxWidth: .infinity)
            .accessibilityIdentifier(identifierPrefix + ".volume")
    }

    private var minusButton: some View {
        stepButton(-1, systemImage: "minus", label: BottleSheetCopy.decreaseAmount, identifier: identifierPrefix + ".minus")
    }

    private var plusButton: some View {
        stepButton(1, systemImage: "plus", label: BottleSheetCopy.increaseAmount, identifier: identifierPrefix + ".plus")
    }

    /// Four amounts on a row at the ordinary sizes, two at the accessibility
    /// sizes, where a quarter of the sheet stacked "120" one digit per line.
    private var chipsGrid: some View {
        let columns = typeSize.isAccessibilitySize ? 2 : chips.count
        return LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: Spacing.s), count: columns), spacing: Spacing.s) {
            ForEach(Array(chips.enumerated()), id: \.offset) { index, ml in
                Button {
                    volumeML = ml
                } label: {
                    Text(label(ml))
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.selectableChip(tint, isSelected: volumeML == ml))
                .accessibilityIdentifier("\(identifierPrefix).chip.\(index)")
                .accessibilityLabel("\(label(ml)) \(unit.shortName)")
            }
        }
    }

    private func stepButton(_ direction: Int, systemImage: String, label: String, identifier: String) -> some View {
        Button {
            step(direction)
        } label: {
            Image(systemName: systemImage).font(.body.weight(.semibold))
        }
        .buttonStyle(.tile(tint, size: .widget))
        .accessibilityLabel(label)
        .accessibilityIdentifier(identifier)
    }
}
