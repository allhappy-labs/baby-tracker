import SwiftUI
import BabyCore
import BabyUI

/// "Pumping · Both · 12:34" and Stop, above the dock. Tapping it (not Stop)
/// opens the pump panel. A caregiver who may not write sees it without Stop.
struct PumpStripView: View {
    let pump: RunningPump
    let canLog: Bool
    let onOpen: () -> Void
    let onStop: () -> Void

    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        // At the accessibility sizes Stop goes under the caption, so the
        // caption keeps the width to wrap by the word.
        let layout = dynamicTypeSize.isAccessibilitySize
            ? AnyLayout(VStackLayout(alignment: .leading, spacing: 0))
            : AnyLayout(HStackLayout(spacing: Spacing.s))
        return Card(padding: Spacing.xs) {
            layout {
                Button(action: onOpen) {
                    HStack(spacing: Spacing.s) {
                        BabyIconView(.pump, size: 18)
                            .foregroundStyle(Palette.pump)
                        (Text(PumpCopy.caption(pump.side) + " · ")
                            + Text(timerInterval: pump.startedAt...Date.distantFuture, countsDown: false))
                            .font(.subheadline.weight(.semibold).monospacedDigit())
                            .foregroundStyle(Palette.primaryText)
                            .fixedSize(horizontal: false, vertical: true)
                        Spacer(minLength: 0)
                    }
                    .frame(minHeight: 44)
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .disabled(!canLog)
                .accessibilityLabel(PumpCopy.caption(pump.side))
                .accessibilityIdentifier("home.pumpOpen")

                if canLog {
                    Button(action: onStop) {
                        Chip(HomeCopy.stop, tint: Palette.pump)
                            .frame(minHeight: 44)
                            .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(HomeCopy.stopPumping)
                    .accessibilityIdentifier("home.pumpStop")
                }
            }
            .padding(.horizontal, Spacing.s)
        }
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("home.pumpStrip")
    }
}
