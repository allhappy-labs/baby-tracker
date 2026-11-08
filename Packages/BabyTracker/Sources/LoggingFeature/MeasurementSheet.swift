import SwiftUI
import Observation
import BabyCore
import BabyStore
import BabyUI

public enum MeasurementSheetMode {
    case create(babyID: UUID)
    case edit(entry: MeasurementCheckIn)

    var babyID: UUID {
        switch self {
        case .create(let id): id
        case .edit(let entry): entry.babyID
        }
    }
}

@MainActor @Observable
public final class MeasurementSheetModel {
    public enum Field: String { case weight, length, head, position, date }

    public func validationMessage(for field: Field) -> String? {
        switch field {
        case .weight, .length, .head:
            let (text, message): (String, String) = switch field {
            case .weight: (weight, "Enter a positive number for weight.")
            case .length: (length, "Enter a positive number for length or height.")
            default: (head, "Enter a positive number for head circumference.")
            }
            do { _ = try number(text); return nil }
            catch { return LoggingL10n.text(message) }
        case .position:
            guard (try? number(length)) != nil, lengthPosition == nil else { return nil }
            return LoggingL10n.text("Choose lying down or standing up.")
        case .date:
            guard let birthDay, let measuredDay else {
                return LoggingL10n.text("The measurement date or birth date is unavailable.")
            }
            if measuredDay < birthDay {
                return LoggingL10n.text("Choose a date on or after the baby’s birth date.")
            }
            if when > latestAllowedDate {
                return LoggingL10n.text("Choose a date and time that is not in the future.")
            }
            return nil
        }
    }

    private let store: BabyStore
    private let mode: MeasurementSheetMode
    private let writeContext: RealmWriteContext?
    private let birthDay: LocalDate?
    public var weight = ""
    public var length = ""
    public var head = ""
    private var chosenPosition: LengthPosition?
    private var hasChosenPosition = false
    public var lengthPosition: LengthPosition? {
        get { hasChosenPosition ? chosenPosition : defaultPosition }
        set { chosenPosition = newValue; hasChosenPosition = true }
    }

    private var measuredDay: LocalDate? {
        guard let zone = TimeZone(identifier: timeZoneID) else { return nil }
        return try? LocalDate(displaying: when, in: zone)
    }

    private var defaultPosition: LengthPosition? {
        guard let birthDay, let measuredDay else { return nil }
        // Compare stored-zone calendar dates with the chronological birthday,
        // independently of due date, leap-year length, and daylight saving.
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(secondsFromGMT: 0)!
        func date(_ day: LocalDate) -> Date? {
            let parts = day.iso8601.split(separator: "-").compactMap { Int($0) }
            guard parts.count == 3 else { return nil }
            return calendar.date(from: DateComponents(year: parts[0], month: parts[1], day: parts[2]))
        }
        guard let birth = date(birthDay), let measured = date(measuredDay),
              let secondBirthday = calendar.date(byAdding: .year, value: 2, to: birth),
              measured >= birth else { return nil }
        return measured < secondBirthday ? .lying : .standing
    }
    public var when: Date
    public let latestAllowedDate: Date
    public let timeZoneID: String
    public private(set) var errorMessage: String?

    public init(store: BabyStore, mode: MeasurementSheetMode) {
        self.store = store
        self.mode = mode
        writeContext = try? store.captureWriteContext()
        let openingTime = store.now
        latestAllowedDate = openingTime
        let baby = try? store.baby(mode.babyID)
        birthDay = baby?.localBirthDate ?? baby.flatMap { try? LocalDate(displaying: $0.birthDate, in: .current) }
        switch mode {
        case .create:
            when = openingTime
            timeZoneID = TimeZone.current.identifier
        case .edit(let entry):
            when = entry.measuredAt
            timeZoneID = entry.timeZoneID
            weight = entry.weightKG.map { String($0) } ?? ""
            length = entry.lengthCM.map { String($0) } ?? ""
            head = entry.headCircumferenceCM.map { String($0) } ?? ""
            chosenPosition = entry.lengthPosition
            hasChosenPosition = true
        }
    }

    /// Accept decimal dots and commas, but never grouping, mixed separators,
    /// nonfinite numbers or a silently ignored invalid field. Scientific
    /// notation preserves valid very small values when an existing entry opens.
    private func number(_ text: String) throws -> Double? {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return nil }
        guard trimmed.range(of: #"^(?:[0-9]+(?:[.,][0-9]*)?|[.,][0-9]+)(?:[eE][+-]?[0-9]+)?$"#, options: .regularExpression) != nil,
              let value = Double(trimmed.replacingOccurrences(of: ",", with: ".")), value.isFinite, value > 0 else {
            throw MeasurementValues.ValidationError.empty
        }
        return value
    }

    private var values: MeasurementValues? {
        do {
            let lengthCM = try number(length)
            return try MeasurementValues(weightKG: number(weight), lengthCM: lengthCM,
                headCircumferenceCM: number(head), lengthPosition: lengthCM == nil ? nil : lengthPosition)
        } catch { return nil }
    }

    public var canSave: Bool {
        guard writeContext != nil, values != nil, when <= latestAllowedDate,
              let birthDay, let zone = TimeZone(identifier: timeZoneID),
              let day = try? LocalDate(displaying: when, in: zone) else { return false }
        return day >= birthDay
    }

    public func save() -> Bool {
        do {
            guard canSave, let values, let writeContext,
                  try store.captureWriteContext() == writeContext else {
                throw GrowthStoreError.measurementUnavailable
            }
            switch mode {
            case .create(let id):
                try store.logMeasurement(for: id, at: when, timeZoneID: timeZoneID, values: values)
            case .edit(let entry):
                try store.editMeasurement(entry, at: when, timeZoneID: timeZoneID, values: values)
            }
            errorMessage = nil
            return true
        } catch {
            errorMessage = LoggingL10n.text("Couldn’t save measurement. Your entry is still here; try again.")
            return false
        }
    }

    /// True when the sheet edits a check-in already logged, the only case
    /// with something to delete.
    public var isEditing: Bool {
        if case .edit = mode { true } else { false }
    }

    /// Tombstones the check-in through the growth store's own delete, which
    /// syncs. Measurements are never in sessions, so this is the right call
    /// (ruling H-R11 is about feeds and sleeps). Refused, like `save()`, once
    /// the active realm is no longer the one the sheet opened in.
    public func delete() throws {
        guard case .edit(let entry) = mode else { throw EditModeError.invalid }
        guard let writeContext, try store.captureWriteContext() == writeContext else {
            throw GrowthStoreError.measurementUnavailable
        }
        try store.softDelete(entry)
    }
}

public struct MeasurementSheet: View {
    @State private var model: MeasurementSheetModel
    private let onDone: () -> Void
    private let title: String
    @Environment(\.errorReporter) private var errorReporter

    public init(store: BabyStore, mode: MeasurementSheetMode, onDone: @escaping () -> Void) {
        _model = State(initialValue: MeasurementSheetModel(store: store, mode: mode))
        self.onDone = onDone
        switch mode {
        case .create: title = LoggingL10n.text("Measure")
        case .edit: title = LoggingL10n.text("Edit measurement")
        }
    }

    public var body: some View {
            EntryPanel(title: title, icon: BabyIcon.growth, tint: Palette.growth, canSave: model.canSave,
                       identifierPrefix: "measurement", onCancel: onDone,
                       onSave: save, onDelete: deleteAction) {
                fields
            }
    }

    @ViewBuilder
    private var fields: some View {
        CardSection {
            field("Weight (kg)", text: $model.weight, field: .weight)
            field("Length / height (cm)", text: $model.length, field: .length)
            if !model.length.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                // Outside a Form a menu picker shows no label: the row
                // names it.
                LabeledRow(LoggingL10n.text("Measured position")) {
                    Picker(LoggingL10n.text("Measured position"), selection: $model.lengthPosition) {
                        Text(LoggingL10n.text("Choose position")).tag(Optional<LengthPosition>.none)
                        Text(LoggingL10n.text("Lying down")).tag(Optional(LengthPosition.lying))
                        Text(LoggingL10n.text("Standing up")).tag(Optional(LengthPosition.standing))
                    }
                    .labelsHidden()
                    .accessibilityIdentifier("measurement.position")
                }
                validation(.position)
            }
            field("Head circumference (cm)", text: $model.head, field: .head)
        } footer: {
            Text(LoggingL10n.text("Enter at least one positive value. For length or height, choose the measured position."))
        }
        CardSection {
            // In the check-in's own zone, so the day it shows is the
            // day the date validation checks.
            EditTimeRow(date: $model.when, latest: model.latestAllowedDate, identifier: "measurement.time")
                .environment(\.timeZone, TimeZone(identifier: model.timeZoneID) ?? .current)
            validation(.date)
        }
        if let error = model.errorMessage {
            InlineError(error).accessibilityIdentifier("measurement.error")
        }
    }

    private func field(_ key: String, text: Binding<String>, field: MeasurementSheetModel.Field) -> some View {
        VStack(alignment: .leading, spacing: Spacing.xs) {
            Text(LoggingL10n.text(key)).font(.subheadline)
            TextField(LoggingL10n.text(key), text: text)
                #if os(iOS)
                .keyboardType(.decimalPad)
                #endif
                .accessibilityLabel(LoggingL10n.text(key))
                .accessibilityIdentifier("measurement." + field.rawValue)
            validation(field)
        }
    }
    private func save() {
        if model.save() { onDone() }
    }

    /// Delete only for a check-in already logged.
    private var deleteAction: (() -> Void)? {
        guard model.isEditing else { return nil }
        return delete
    }

    private func delete() {
        if performEdit(model.delete, failure: ErrorBannerCopy.deleteFailed(what: ErrorSubject.entry),
                       reporter: errorReporter) { onDone() }
    }

    @ViewBuilder
    private func validation(_ field: MeasurementSheetModel.Field) -> some View {
        if let message = model.validationMessage(for: field) {
            InlineError(message)
                .accessibilityIdentifier("measurement.validation." + field.rawValue)
        }
    }

}
