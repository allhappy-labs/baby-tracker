import SwiftUI
import Observation
import BabyCore
import BabyStore
import BabyUI

/// One part's editor in the Past entry layout: a breast side's side, start
/// and duration; a bottle's amount chips, contents and time; or a sleep's
/// start, end and Nap / Night / Auto — each with the note under Details, and
/// Save and Delete at the bottom.
///
/// Opened on its own from History it brings its own navigation bar, Cancel
/// and the Join row (`showsJoinRow`, the default). Pushed inside the session
/// sheet, which has the Join row already, it lives in that sheet's
/// navigation stack and returns to the session with `onDone`.
public struct PartEditSheet: View {
    @State private var model: PartEditModel
    @State private var join: JoinRowModel?
    @State private var isShowingDetails: Bool
    /// `onDone` runs once even when a delete is followed by the revision
    /// bump that marks the part gone.
    @State private var isFinished = false
    private let isStandalone: Bool
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(model: PartEditModel, showsJoinRow: Bool = true, onDone: @escaping () -> Void) {
        _model = State(initialValue: model)
        _join = State(initialValue: showsJoinRow
            ? JoinRowModel(store: model.store, babyID: model.babyID, kind: model.target.sessionKind,
                           partOf: model.target.entryID)
            : nil)
        _isShowingDetails = State(initialValue: !model.note.isEmpty)
        isStandalone = showsJoinRow
        self.onDone = onDone
    }

    public var body: some View {
        if isStandalone {
            NavigationStack {
                form.sheetHeight(height)
            }
        } else {
            form
        }
    }

    private var form: some View {
        Form {
            if let join, join.previous != nil {
                Section { JoinRow(model: join) }
            }

            switch model.target {
            case .breast: breastSection
            case .bottle: bottleSections
            case .sleep: sleepSection
            }

            Section {
                DisclosureGroup(isExpanded: $isShowingDetails) {
                    TextField(PartEditCopy.noteLabel, text: $model.note, prompt: Text(PartEditCopy.notePrompt))
                        .accessibilityLabel(PartEditCopy.noteLabel)
                        .accessibilityIdentifier("partEdit.note")
                } label: {
                    Text(PartEditCopy.detailsLabel)
                        .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                        .accessibilityIdentifier("partEdit.details")
                }
            }
        }
        .editFooter(canSave: model.canSave, identifierPrefix: "partEdit", onSave: save, onDelete: delete)
        .navigationTitle(title)
        .editTitle(icon: icon, tint: tint, label: title)
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .toolbar {
            if isStandalone {
                ToolbarItem(placement: .cancellationAction) {
                    Button(PartEditCopy.cancel, action: onDone)
                }
            }
        }
        .editLifecycle(revision: model.store.revision, isGone: model.isGone,
                       reload: { model.reload(); join?.reload() }, onGone: finish)
    }

    // MARK: - Breast

    private var breastSection: some View {
        Section {
            Picker(PartEditCopy.sideLabel, selection: $model.side) {
                Text(BreastSide.left.displayName).tag(BreastSide.left)
                Text(BreastSide.right.displayName).tag(BreastSide.right)
            }
            .pickerStyle(.segmented)
            .accessibilityIdentifier("partEdit.side")

            EditTimeRow(PartEditCopy.startLabel, date: $model.start, latest: model.latestAllowedDate,
                        identifier: "partEdit.start")

            // From 30 s, or from an old part's shorter stored length, so
            // opening one does not clamp it.
            Stepper(value: $model.durationSeconds, in: min(30, model.durationSeconds)...max(4 * 3600, model.durationSeconds),
                    step: 60) {
                HStack {
                    Text(PartEditCopy.durationLabel)
                    Spacer()
                    Text(ElapsedFormatter.precise(model.durationSeconds))
                        .foregroundStyle(Palette.secondaryText)
                        .monospacedDigit()
                }
            }
            .frame(minHeight: 44)
            .accessibilityElement(children: .combine)
            .accessibilityIdentifier("partEdit.duration")
        } footer: {
            validationFooter
        }
    }

    // MARK: - Bottle

    @ViewBuilder
    private var bottleSections: some View {
        Section {
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

            chips

            HStack(spacing: 8) {
                stepButton(-1, systemImage: "minus", label: BottleSheetCopy.decreaseAmount, identifier: "partEdit.minus")
                stepButton(1, systemImage: "plus", label: BottleSheetCopy.increaseAmount, identifier: "partEdit.plus")
            }
        }

        Section {
            Picker(BottleSheetCopy.contentsLabel, selection: $model.contents) {
                Text(BottleSheetCopy.breastMilk).tag(BottleContents.breastMilk)
                Text(BottleSheetCopy.formula).tag(BottleContents.formula)
                Text(BottleSheetCopy.mixed).tag(BottleContents.mixed)
            }
            .pickerStyle(.segmented)
            .accessibilityIdentifier("partEdit.contents")

            EditTimeRow(date: $model.start, latest: model.latestAllowedDate, identifier: "partEdit.time")
        } footer: {
            validationFooter
        }
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

    // MARK: - Sleep

    private var sleepSection: some View {
        Section {
            EditTimeRow(PartEditCopy.startLabel, date: $model.start, latest: model.latestAllowedDate,
                        identifier: "partEdit.start")
            EditTimeRow(PartEditCopy.endLabel, date: $model.end, latest: model.latestAllowedDate,
                        identifier: "partEdit.end")
            Picker(PartEditCopy.kindLabel, selection: $model.sleepKind) {
                Text(PartEditCopy.nap).tag(Optional(SleepKind.nap))
                Text(PartEditCopy.night).tag(Optional(SleepKind.night))
                Text(PartEditCopy.auto).tag(SleepKind?.none)
            }
            .pickerStyle(.segmented)
            .accessibilityIdentifier("partEdit.kind")
        } footer: {
            validationFooter
        }
    }

    // MARK: - Shared

    @ViewBuilder
    private var validationFooter: some View {
        if let message = model.validationMessage {
            Text(message)
                .foregroundStyle(Palette.destructive)
                .fixedSize(horizontal: false, vertical: true)
                .accessibilityIdentifier("partEdit.validation")
        }
    }

    private var title: String {
        switch model.target {
        case .breast: PartEditCopy.breastTitle
        case .bottle: PartEditCopy.bottleTitle
        case .sleep: Tracker.sleep.title
        }
    }

    private var icon: BabyIcon {
        switch model.target {
        case .breast: model.side == .left ? .breastLeft : .breastRight
        case .bottle: .bottle
        case .sleep: .sleep
        }
    }

    private var tint: Color { model.target.sessionKind == .feed ? Palette.feed : Palette.sleep }

    /// Sized to the form at the default text size: the kind's rows, Details,
    /// the Join row when there is one, a validation line when there is one,
    /// and the footer.
    private var height: CGFloat {
        // Measured from the Task 7 screenshots: with 300 / 440 the breast
        // part's Duration, the sleep's Nap / Night / Auto and the bottle's
        // contents, time and Details sat under the footer when the editor
        // opened on its own.
        let rows: CGFloat = switch model.target {
        case .breast: 460
        case .bottle: 650
        case .sleep: 460
        }
        return rows
            + (join?.previous == nil ? 0 : 64)
            + (isShowingDetails ? 52 : 0)
            + (model.validationMessage == nil ? 0 : 44)
            + EditFooter.height
    }

    // MARK: - Actions

    private func save() {
        var saved = false
        let subject = model.target.sessionKind == .feed ? ErrorSubject.feed : ErrorSubject.sleep
        // A refused draft keeps the sheet open with its message in the form.
        if performEdit({ saved = try model.save() }, failure: ErrorBannerCopy.saveFailed(what: subject),
                       reporter: errorReporter), saved || model.isGone {
            finish()
        }
    }

    private func delete() {
        let subject = model.target.sessionKind == .feed ? ErrorSubject.feed : ErrorSubject.sleep
        if performEdit(model.delete, failure: ErrorBannerCopy.deleteFailed(what: subject), reporter: errorReporter) {
            finish()
        }
    }

    private func finish() {
        guard !isFinished else { return }
        isFinished = true
        onDone()
    }
}
