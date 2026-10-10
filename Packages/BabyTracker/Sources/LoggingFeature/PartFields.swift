import SwiftUI
import BabyCore
import BabyUI

/// The part panel's rows for one kind, then its Start and End, then Details —
/// the same in new mode and edit mode.
struct PartFields<Model: PartFormModel>: View {
    @Bindable var model: Model
    @Binding var isShowingDetails: Bool
    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        switch model.kind {
        case .breast:
            PanelSection {
                Picker(PartEditCopy.sideLabel, selection: $model.side) {
                    Text(BreastSide.left.displayName).tag(BreastSide.left)
                    Text(BreastSide.right.displayName).tag(BreastSide.right)
                }
                .pickerStyle(.segmented)
                .accessibilityIdentifier("partEdit.side")
            }
        case .bottle:
            bottleSection
        case .sleep:
            PanelSection {
                Picker(PartEditCopy.kindLabel, selection: $model.sleepKind) {
                    Text(PartEditCopy.nap).tag(Optional(SleepKind.nap))
                    Text(PartEditCopy.night).tag(Optional(SleepKind.night))
                    Text(PartEditCopy.auto).tag(SleepKind?.none)
                }
                .pickerStyle(.segmented)
                .accessibilityIdentifier("partEdit.kind")
            }
        }

        TimeSpanSection(start: $model.start, end: $model.end,
                        endStyle: model.kind == .bottle ? .optional : .required,
                        latest: model.latestAllowedDate, identifierPrefix: "partEdit") {
            if let message = model.validationMessage {
                Text(message)
                    .foregroundStyle(Palette.destructive)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityIdentifier("partEdit.validation")
            }
        }

        DetailsSection(isExpanded: $isShowingDetails, identifier: "partEdit.details") {
            DetailFieldRow(EditChromeCopy.note, text: $model.note, identifier: "partEdit.note")
        }
    }

    // MARK: - Bottle

    @ViewBuilder
    private var bottleSection: some View {
        PanelSection {
            if typeSize.isAccessibilitySize {
                volume
                chips
                HStack(spacing: 8) {
                    minusButton.frame(maxWidth: .infinity)
                    plusButton.frame(maxWidth: .infinity)
                }
            } else {
                // − and + flank the amount rather than taking a row of their
                // own, so a bottle part fits the screen without scrolling.
                HStack(spacing: 8) {
                    minusButton.frame(width: 64)
                    volume
                    plusButton.frame(width: 64)
                }
                chips
            }
        }

        PanelSection {
            Picker(BottleSheetCopy.contentsLabel, selection: $model.contents) {
                Text(BottleSheetCopy.breastMilk).tag(BottleContents.breastMilk)
                Text(BottleSheetCopy.formula).tag(BottleContents.formula)
                Text(BottleSheetCopy.mixed).tag(BottleContents.mixed)
            }
            .pickerStyle(.segmented)
            .accessibilityIdentifier("partEdit.contents")
        }
    }

    private var volume: some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Text(model.label(for: model.volumeML))
                .numeralStyle(.display)
            Text(model.unit.shortName)
                .font(.title3)
                .foregroundStyle(Palette.secondaryText)
        }
        .frame(maxWidth: .infinity)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(BottleSheetCopy.volumeLabel)
        .accessibilityValue("\(model.label(for: model.volumeML)) \(model.unit.shortName)")
        .accessibilityIdentifier("partEdit.volume")
    }

    private var minusButton: some View {
        stepButton(-1, systemImage: "minus", label: BottleSheetCopy.decreaseAmount, identifier: "partEdit.minus")
    }

    private var plusButton: some View {
        stepButton(1, systemImage: "plus", label: BottleSheetCopy.increaseAmount, identifier: "partEdit.plus")
    }

    /// Four amounts on a row at the ordinary sizes, two at the accessibility
    /// sizes, as on the Bottle sheet. A tap dials the amount in; Save writes.
    private var chips: some View {
        let columns = typeSize.isAccessibilitySize ? 2 : model.chips.count
        return LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 8), count: columns), spacing: 8) {
            ForEach(Array(model.chips.enumerated()), id: \.offset) { index, ml in
                Button {
                    model.volumeML = ml
                } label: {
                    Text(model.label(for: ml))
                        .font(.body.weight(.bold))
                        .frame(maxWidth: .infinity, minHeight: 44)
                }
                .buttonStyle(.tracker(Palette.feed, emphasis: model.volumeML == ml ? .primary : .secondary))
                .accessibilityIdentifier("partEdit.chip.\(index)")
                .accessibilityLabel("\(model.label(for: ml)) \(model.unit.shortName)")
            }
        }
    }

    private func stepButton(_ direction: Int, systemImage: String, label: String, identifier: String) -> some View {
        Button {
            model.step(direction)
        } label: {
            Image(systemName: systemImage).frame(maxWidth: .infinity, minHeight: 44)
        }
        .buttonStyle(.tracker(Palette.secondaryText))
        .accessibilityLabel(label)
        .accessibilityIdentifier(identifier)
    }
}
