import SwiftUI
import BabyStore
import BabyUI

/// "Same feed · L 9:02 ✕": the running part joined the session before it.
/// ✕ makes the running part a session of its own, immediately.
///
/// The kit's `Chip` with a label of its own: the shared `PartsRow` after the
/// "Same feed" text, read as one element; the ✕ is the chip's remove control.
struct JoinChipView: View {
    let chip: JoinChip
    /// Nil for a caregiver who may not write.
    let onDetach: ((SessionKind) -> Void)?

    var body: some View {
        Chip(
            onRemove: onDetach.map { detach in { detach(chip.kind) } },
            removeLabel: HomeCopy.makeSeparate(chip.kind),
            removeIdentifier: "home.joinChip.separate"
        ) {
            HStack(spacing: Spacing.s) {
                Text(HomeCopy.same(chip.kind) + " ·")
                PartsRow(chip.previousParts.map(\.partsRowItem))
            }
            .accessibilityElement(children: .ignore)
            // Read, never tapped — ✕ beside it is the control.
            .accessibilityAddTraits(.isStaticText)
            .accessibilityLabel(([HomeCopy.same(chip.kind)] + chip.previousParts.map(HomeCopy.spokenPart))
                .joined(separator: ", "))
            .accessibilityIdentifier("home.joinChip")
        }
    }
}
