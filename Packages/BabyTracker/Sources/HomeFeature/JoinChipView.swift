import SwiftUI
import BabyStore
import BabyUI

/// "Same feed · L 9:02 ✕": the running part joined the session before it.
/// ✕ makes the running part a session of its own, immediately.
struct JoinChipView: View {
    let chip: JoinChip
    /// Nil for a caregiver who may not write.
    let onDetach: ((SessionKind) -> Void)?

    var body: some View {
        HStack(spacing: 8) {
            HStack(spacing: 8) {
                Text(HomeCopy.same(chip.kind) + " ·")
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(Palette.primaryText)
                PartsRow(parts: chip.previousParts, iconSize: 14)
            }
            .padding(.leading, 14)
            .padding(.trailing, onDetach == nil ? 14 : 0)
            .accessibilityElement(children: .ignore)
            // Read, never tapped — ✕ beside it is the control.
            .accessibilityAddTraits(.isStaticText)
            .accessibilityLabel(([HomeCopy.same(chip.kind)] + chip.previousParts.map(HomeCopy.spokenPart))
                .joined(separator: ", "))
            .accessibilityIdentifier("home.joinChip")

            if let onDetach {
                Button { onDetach(chip.kind) } label: {
                    BabyIconView(.close, size: 12)
                        .foregroundStyle(Palette.primaryText)
                        .padding(6)
                        .background(.white.opacity(0.12), in: Circle())
                        .frame(minWidth: 44, minHeight: 44)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel(HomeCopy.makeSeparate(chip.kind))
                .accessibilityIdentifier("home.joinChip.separate")
            }
        }
        .frame(minHeight: 44)
        .background(.white.opacity(0.07), in: Capsule())
    }
}
