import SwiftUI
import BabyCore
import BabyUI

/// The part panel's rows for one kind, then its Start and End, then Details —
/// the same in new mode and edit mode.
struct PartFields<Model: PartFormModel>: View {
    @Bindable var model: Model
    @Binding var isShowingDetails: Bool

    var body: some View {
        switch model.kind {
        case .breast:
            // No card behind a segmented control: it is a container already.
            Picker(PartEditCopy.sideLabel, selection: $model.side) {
                Text(BreastSide.left.displayName).tag(BreastSide.left)
                Text(BreastSide.right.displayName).tag(BreastSide.right)
            }
            .pickerStyle(.segmented)
            .accessibilityIdentifier("partEdit.side")
        case .bottle:
            bottleSection
        case .sleep:
            Picker(PartEditCopy.kindLabel, selection: $model.sleepKind) {
                Text(PartEditCopy.nap).tag(Optional(SleepKind.nap))
                Text(PartEditCopy.night).tag(Optional(SleepKind.night))
                Text(PartEditCopy.auto).tag(SleepKind?.none)
            }
            .pickerStyle(.segmented)
            .accessibilityIdentifier("partEdit.kind")
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
        BottleAmountSection(volumeML: $model.volumeML, unit: model.unit, chips: model.chips,
                            label: model.label(for:), step: model.step, identifierPrefix: "partEdit")

        Picker(BottleSheetCopy.contentsLabel, selection: $model.contents) {
            Text(BottleSheetCopy.breastMilk).tag(BottleContents.breastMilk)
            Text(BottleSheetCopy.formula).tag(BottleContents.formula)
            Text(BottleSheetCopy.mixed).tag(BottleContents.mixed)
        }
        .pickerStyle(.segmented)
        .accessibilityIdentifier("partEdit.contents")
    }
}
