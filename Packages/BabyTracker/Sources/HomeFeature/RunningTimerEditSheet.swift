import SwiftUI
import BabyCore
import BabyUI

/// Correcting a timer while it still runs: the start (tapped Start late, or
/// forgot and started it ten minutes in) and the parts already finished — a
/// part's Start and End, a bottle's volume and contents. The side running
/// now is shown with its Start and an End of "Running", not editable; it is
/// still being measured.
///
/// Opened from Home's hero and from a running row in History (ruling H-R5),
/// which `RootView` presents. Every string comes from `RunningTimerEditModel`
/// and `RunningTimerEditCopy`.
public struct RunningTimerEditSheet: View {
    @Bindable var model: RunningTimerEditModel
    let onDone: () -> Void

    public init(model: RunningTimerEditModel, onDone: @escaping () -> Void) {
        self.model = model
        self.onDone = onDone
    }

    @Environment(\.errorReporter) private var errorReporter
    @State private var isSaving = false

    private var tint: Color { model.isSleep ? Palette.sleep : Palette.feed }

    /// Presented with `fittedSheet`: the sheet is as tall as this, so Cancel,
    /// the title and Save are drawn here rather than in a navigation bar.
    public var body: some View {
        VStack(spacing: 0) {
            SheetHeader(cancelTitle: model.cancelLabel, onCancel: onDone) {
                SheetTitleText(model.title)
            } trailing: {
                Button(model.saveLabel, action: save)
                    .buttonStyle(.glassProminent)
                    .controlSize(.large)
                    .disabled(!model.canSave || isSaving)
            }

            ScrollView {
                VStack(spacing: 12) {
                    if let message = model.validationMessage {
                        PanelSection {
                            Label {
                                Text(message)
                            } icon: {
                                Image(systemName: "exclamationmark.circle.fill")
                                    .foregroundStyle(Palette.destructive)
                                    .accessibilityHidden(true)
                            }
                            .frame(minHeight: 44, alignment: .leading)
                            .accessibilityIdentifier("timerEdit.message")
                        }
                    }

                    startSection

                    // The finished parts and the running side in one section,
                    // in the order they happened: what is editable, then the
                    // side still being measured.
                    if !model.parts.isEmpty || model.runningPart != nil {
                        VStack(alignment: .leading, spacing: 6) {
                            Text(model.parts.isEmpty ? RunningTimerEditCopy.nowHeader : RunningTimerEditCopy.soFarHeader)
                                .font(.footnote)
                                .foregroundStyle(Palette.secondaryText)
                                .padding(.horizontal, 16)
                            PanelSection {
                                ForEach($model.parts) { $part in
                                    partRows($part)
                                }
                                if let running = model.runningPart {
                                    Text(running.title).font(.body.weight(.semibold))
                                    LabeledContent(RunningTimerEditCopy.partStart, value: model.runningStartText ?? "")
                                        .monospacedDigit()
                                    LabeledContent(RunningTimerEditCopy.partEnd, value: RunningTimerEditCopy.running)
                                        .accessibilityIdentifier("timerEdit.runningSide")
                                }
                            } footer: {
                                if model.runningPart != nil {
                                    Text(RunningTimerEditCopy.nowFooter)
                                }
                            }
                        }
                    }
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 12)
            }
            .scrollBounceBehavior(.basedOnSize)
        }
        .tint(tint)
    }

    private var startSection: some View {
        PanelSection {
            // Date as well as time: a feed started before midnight and
            // corrected after it is on the other day.
            DatePicker(
                RunningTimerEditCopy.startLabel,
                selection: $model.startedAt,
                in: model.startRange,
                displayedComponents: [.date, .hourAndMinute]
            )
            .frame(minHeight: 44)
            .accessibilityIdentifier("timerEdit.start")

            HStack(spacing: 12) {
                nudge(
                    RunningTimerEditCopy.startEarlierShort,
                    label: RunningTimerEditCopy.startEarlier,
                    identifier: "timerEdit.earlier",
                    by: -300
                )
                nudge(
                    RunningTimerEditCopy.startLaterShort,
                    label: RunningTimerEditCopy.startLater,
                    identifier: "timerEdit.later",
                    by: 300
                )
            }
        }
    }

    /// Two equal buttons side by side in one row. `.borderless` so a tap on
    /// one does not fire the whole Form row, which is what a bordered button
    /// in a List row does.
    private func nudge(_ title: String, label: String, identifier: String, by seconds: TimeInterval) -> some View {
        Button {
            model.shiftStart(by: seconds)
        } label: {
            Text(title)
                .font(.body.weight(.semibold))
                .monospacedDigit()
                .frame(maxWidth: .infinity, minHeight: 44)
                .background(tint.opacity(0.14), in: .rect(cornerRadius: 10))
        }
        .buttonStyle(.borderless)
        .accessibilityLabel(label)
        .accessibilityIdentifier(identifier)
    }

    /// A finished part: its name, Start and End (date and time — a part
    /// can be before midnight), the length they make, and a bottle's volume
    /// and contents. An old part recorded without times shows its length
    /// only.
    @ViewBuilder
    private func partRows(_ part: Binding<RunningTimerEditModel.Part>) -> some View {
        let value = part.wrappedValue
        Text(value.title).font(.body.weight(.semibold))
        if value.start != nil, value.end != nil {
            timeRow(RunningTimerEditCopy.partStart, date: Binding(
                get: { part.wrappedValue.start ?? .now },
                set: { part.wrappedValue.start = $0 }
            ), identifier: "timerEdit.part.\(value.id).start",
               accessibilityLabel: RunningTimerEditCopy.partStartAccessibilityLabel(value.title))
            timeRow(RunningTimerEditCopy.partEnd, date: Binding(
                get: { part.wrappedValue.end ?? .now },
                set: { part.wrappedValue.end = $0 }
            ), identifier: "timerEdit.part.\(value.id).end",
               accessibilityLabel: RunningTimerEditCopy.partEndAccessibilityLabel(value.title))
        }
        if let length = model.lengthText(for: value) {
            LabeledContent(RunningTimerEditCopy.length, value: length)
                .monospacedDigit()
        }
        if case .bottle = value.kind {
            bottleRows(part)
        }
    }

    /// Back as far as the start picker goes, forward to when the running
    /// part began (or now). Whether the times fit the feed is the model's to
    /// say, with a message. Named for VoiceOver with the part it belongs to,
    /// since the visible title alone is just "Start"/"End".
    private func timeRow(_ title: String, date: Binding<Date>, identifier: String, accessibilityLabel: String) -> some View {
        DatePicker(
            title,
            selection: date,
            in: model.startRange.lowerBound...max(model.startRange.lowerBound, model.latestPartEnd),
            displayedComponents: [.date, .hourAndMinute]
        )
        .frame(minHeight: 44)
        .accessibilityLabel(accessibilityLabel)
        .accessibilityIdentifier(identifier)
    }

    @ViewBuilder
    private func bottleRows(_ part: Binding<RunningTimerEditModel.Part>) -> some View {
        let value = part.wrappedValue
        partRow(value.title, detail: model.volumeText(for: value)) {
            HStack(spacing: 8) {
                BottleVolumeField(
                    volumeML: part.volumeML,
                    unit: model.unit,
                    label: RunningTimerEditCopy.volumeAccessibilityLabel(value.title)
                )
                Stepper(
                    RunningTimerEditCopy.volumeAccessibilityLabel(value.title),
                    value: part.volumeML,
                    in: 0...max(RunningTimerEditModel.volumeRange.upperBound, value.volumeML + model.volumeStep),
                    step: model.volumeStep
                )
                .accessibilityValue(model.volumeText(for: value))
            }
        }

        // Outside a Form a menu picker shows no label: the row names it.
        LabeledContent(RunningTimerEditCopy.contentsLabel) {
            Picker(RunningTimerEditCopy.contentsLabel, selection: part.contents) {
                ForEach(BottleContents.allCases, id: \.self) { contents in
                    Text(contents.displayName).tag(contents)
                }
            }
            .labelsHidden()
            .accessibilityLabel(RunningTimerEditCopy.contentsAccessibilityLabel(value.title))
        }
        .frame(minHeight: 44)
    }

    /// The visible name and figure, with the control's own label hidden and
    /// named for VoiceOver instead. A Stepper's content label is read AND its
    /// accessibility label appended, so "Left breast, 6s, Left breast
    /// duration, 6s" was what it said; this reads "Left breast duration, 6s".
    private func partRow<Control: View>(
        _ title: String,
        detail: String,
        @ViewBuilder control: () -> Control
    ) -> some View {
        HStack(spacing: 12) {
            partLabel(title, detail: detail)
                .accessibilityHidden(true)
            Spacer(minLength: 0)
            control()
                .labelsHidden()
                .fixedSize()
        }
        .frame(minHeight: 44)
    }

    /// The part's name over its figure, left-aligned like the running
    /// screen's action rows, so a long name wraps rather than truncating at
    /// large text sizes.
    private func partLabel(_ title: String, detail: String) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title).font(.body.weight(.semibold))
            Text(detail)
                .font(.subheadline)
                .monospacedDigit()
                .foregroundStyle(Palette.secondaryText)
        }
    }

    private func save() {
        isSaving = true
        Task {
            defer { isSaving = false }
            do {
                if try await model.save() {
                    errorReporter.clear()
                    onDone()
                }
                // A refusal keeps the sheet open with the reason on it, and
                // the user's edits intact.
            } catch {
                errorReporter.report(
                    ErrorBannerCopy.saveFailed(what: model.isSleep ? ErrorSubject.sleep : ErrorSubject.feed),
                    error: error,
                    category: AppLog.store
                )
            }
        }
    }
}
