import SwiftUI
import BabyCore
import BabyUI

/// Correcting a timer while it still runs: the start (tapped Start late, or
/// forgot and started it ten minutes in) and the parts already finished — a
/// side's length, a bottle's volume and contents. The side running now is
/// shown but not editable; it is still being measured.
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

    public var body: some View {
        NavigationStack {
            Form {
                if let message = model.validationMessage {
                    Section {
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

                // The finished parts and the running side in one section, in
                // the order they happened: what is editable, then the side
                // still being measured.
                if !model.parts.isEmpty || model.runningSideText != nil {
                    Section {
                        ForEach($model.parts) { $part in
                            partRows($part)
                        }
                        if let running = model.runningSideText {
                            Text(running)
                                .foregroundStyle(Palette.secondaryText)
                                .frame(minHeight: 44, alignment: .leading)
                                .accessibilityIdentifier("timerEdit.runningSide")
                        }
                    } header: {
                        Text(model.parts.isEmpty ? RunningTimerEditCopy.nowHeader : RunningTimerEditCopy.soFarHeader)
                    } footer: {
                        if model.runningSideText != nil {
                            Text(RunningTimerEditCopy.nowFooter)
                        }
                    }
                }

            }
            .tint(tint)
            // As tall as what is on it. At the default detent a fresh feed's
            // editor was more than half empty below one line of content.
            .sheetHeight(model.sheetHeight)
            .navigationTitle(model.title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(model.cancelLabel, action: onDone)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button(model.saveLabel, action: save)
                        .disabled(!model.canSave || isSaving)
                }
            }
        }
    }

    private var startSection: some View {
        Section {
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

    @ViewBuilder
    private func partRows(_ part: Binding<RunningTimerEditModel.Part>) -> some View {
        let value = part.wrappedValue
        switch value.kind {
        case .breast:
            partRow(value.title, detail: model.durationText(for: value)) {
                Stepper(
                    RunningTimerEditCopy.durationAccessibilityLabel(value.title),
                    value: part.seconds,
                    in: model.durationRange(for: value),
                    step: 60
                )
                .accessibilityValue(model.durationText(for: value))
            }
        case .bottle:
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

            Picker(selection: part.contents) {
                ForEach(BottleContents.allCases, id: \.self) { contents in
                    Text(contents.displayName).tag(contents)
                }
            } label: {
                Text(RunningTimerEditCopy.contentsLabel)
            }
            .frame(minHeight: 44)
            .accessibilityLabel(RunningTimerEditCopy.contentsAccessibilityLabel(value.title))
        }
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
