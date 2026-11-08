import SwiftUI
import BabyCore
import BabyUI

/// Why an edit model refused to write.
public enum EditModeError: Error, Equatable {
    /// The entry is no longer there, for example after the other phone
    /// deleted it while this sheet was open.
    case gone
    /// The draft cannot be saved as it stands: a time in the future, or no
    /// foods on a solids entry. The sheet's Save is off in that state, so
    /// this is the model refusing on its own account rather than trusting it.
    case invalid
}

/// The strings every edit mode shares.
public enum EditChromeCopy {
    public static let save = LoggingL10n.text("Save")
    public static let delete = LoggingL10n.text("Delete")
    public static let deleteConfirmTitle = LoggingL10n.text("Delete this entry?")
    public static let keep = LoggingL10n.text("Keep it")
    public static let timeLabel = LoggingL10n.text("Date and time")
    public static let optional = LoggingL10n.text("Optional")
    public static let note = LoggingL10n.text("Note")
    public static let expanded = LoggingL10n.text("Expanded")
    public static let collapsed = LoggingL10n.text("Collapsed")
    public static let cancel = LoggingL10n.text("Cancel")
    /// A panel picked from Past entry's list goes back to it.
    public static let back = LoggingL10n.text("Back")
    public static let details = LoggingL10n.text("Details")
}

/// An edited entry's time, with its date: an entry is often corrected the day
/// after, and a feed that started before midnight belongs to the day before
/// (ruling H-R7). Never later than `latest`, which callers set to now.
///
/// Two pills, "Today" and "23:15", each opening its wheel under the row. Not
/// the system's compact picker: inserted into a fitted sheet already on
/// screen — Past entry's form, a bottle's End — it kept a stale layout, its
/// date and time drawn over each other.
///
/// `accessory` sits after the pills, on their line — a bottle End's ✕. Beside
/// the whole row it squeezed the wheel, which then pushed the panel wider than
/// the screen.
public struct EditTimeRow<Accessory: View>: View {
    private let label: String
    private let accessory: Accessory
    @Binding private var date: Date
    private let latest: Date
    private let identifier: String
    /// The pill whose wheel is open, if either, when no panel shares one.
    @State private var ownOpen: Part?
    /// The panel's one open wheel: opening this row's closes any other.
    @Environment(\.openTimeWheel) private var sharedOpen

    private enum Part: String { case date, time }

    private var open: Part? {
        guard let sharedOpen else { return ownOpen }
        guard let key = sharedOpen.wrappedValue, key.hasPrefix(identifier + ".") else { return nil }
        return Part(rawValue: String(key.dropFirst(identifier.count + 1)))
    }

    private func setOpen(_ part: Part?) {
        if let sharedOpen {
            sharedOpen.wrappedValue = part.map { identifier + "." + $0.rawValue }
        } else {
            ownOpen = part
        }
    }

    public init(_ label: String = EditChromeCopy.timeLabel, date: Binding<Date>, latest: Date, identifier: String,
                @ViewBuilder accessory: () -> Accessory) {
        self.label = label
        self.accessory = accessory()
        _date = date
        self.latest = latest
        self.identifier = identifier
    }

    @Environment(\.dynamicTypeSize) private var typeSize

    public var body: some View {
        VStack(spacing: 0) {
            if typeSize.isAccessibilitySize {
                // The label over the date and the time: beside them, at these
                // sizes, it was squeezed to "D…".
                VStack(alignment: .leading, spacing: 6) {
                    Text(label)
                        .accessibilityHidden(true)
                    pills
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            } else {
                HStack(spacing: 8) {
                    Text(label)
                        .foregroundStyle(Palette.primaryText)
                        .accessibilityHidden(true)
                    Spacer(minLength: 0)
                    pills
                }
            }
            if let open {
                DatePicker(label, selection: $date, in: ...latest,
                           displayedComponents: open == .date ? .date : .hourAndMinute)
                    #if os(iOS)
                    .datePickerStyle(.wheel)
                    #endif
                    .labelsHidden()
                    .frame(maxWidth: .infinity)
                    .accessibilityIdentifier(identifier + (open == .date ? ".dateWheel" : ".timeWheel"))
            }
        }
        .frame(minHeight: 44)
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier(identifier)
    }

    /// Side by side while both fit at their natural width; otherwise stacked.
    /// A pill never truncates.
    private var pills: some View {
        ViewThatFits(in: .horizontal) {
            HStack(spacing: 6) { pillViews }
            VStack(alignment: .leading, spacing: 6) { pillViews }
        }
    }

    @ViewBuilder private var pillViews: some View {
        pill(.date, text: EditTimeText.dayText(date))
        pill(.time, text: date.formatted(date: .omitted, time: .shortened))
        accessory
    }

    private func pill(_ part: Part, text: String) -> some View {
        let isOpen = open == part
        return Button {
            withAnimation(.snappy) { setOpen(isOpen ? nil : part) }
        } label: {
            Text(text)
                .monospacedDigit()
                .foregroundStyle(isOpen ? AnyShapeStyle(.tint) : AnyShapeStyle(Palette.primaryText))
                .fixedSize(horizontal: true, vertical: false)
                .padding(.horizontal, 10)
                .padding(.vertical, 6)
                .background(Surface.raised, in: .rect(cornerRadius: Radius.control, style: .continuous))
                .frame(minHeight: 44)
                .contentShape(.rect)
        }
        .buttonStyle(.plain)
        .accessibilityLabel(label)
        .accessibilityValue(text)
        .accessibilityAddTraits(isOpen ? .isSelected : [])
        .accessibilityIdentifier(identifier + (part == .date ? ".date" : ".time"))
    }
}

/// The date pill's text. Not on `EditTimeRow`: a generic type's static needs
/// its accessory named to be called.
enum EditTimeText {
    /// "Today", "Yesterday", then the date, in the phone's language.
    static func dayText(_ date: Date) -> String {
        let formatter = DateFormatter()
        formatter.dateStyle = .medium
        formatter.timeStyle = .none
        formatter.doesRelativeDateFormatting = true
        return formatter.string(from: date)
    }
}

extension EditTimeRow where Accessory == EmptyView {
    public init(_ label: String = EditChromeCopy.timeLabel, date: Binding<Date>, latest: Date, identifier: String) {
        self.init(label, date: date, latest: latest, identifier: identifier) { EmptyView() }
    }
}

extension EnvironmentValues {
    /// Which time row's wheel is open in a panel — its identifier and the
    /// pill — so only one is open at a time. Nil outside a panel.
    @Entry var openTimeWheel: Binding<String?>?
}

/// The Save and Delete footer actions every edit and log sheet shares, so no
/// sheet builds them by hand: `<prefix>.save` (disabled unless `canSave`) and
/// `<prefix>.delete`. Delete only exists next to Save: no Save, no footer, so
/// no Delete either. Nothing here deletes; `onDelete` is whatever asks.
public struct EditActions {
    public let primary: SheetAction?
    public let destructive: SheetAction?

    public init(identifierPrefix: String, canSave: Bool, onSave: (() -> Void)?, onDelete: (() -> Void)?) {
        primary = onSave.map {
            SheetAction(EditChromeCopy.save, identifier: identifierPrefix + ".save", isEnabled: canSave, action: $0)
        }
        destructive = onSave == nil ? nil : onDelete.map {
            SheetAction(EditChromeCopy.delete, identifier: identifierPrefix + ".delete", action: $0)
        }
    }
}

/// What every edit mode does about the store changing under it: reload on
/// every revision (the other phone's edits and deletes arrive as one), and
/// close once the entry is gone, including when it opens on one already gone.
private struct EditLifecycle: ViewModifier {
    let isPresented: Bool
    let revision: Int
    let isGone: Bool
    let reload: () -> Void
    let onGone: () -> Void

    func body(content: Content) -> some View {
        if isPresented {
            content
                .onChange(of: revision) { _, _ in reload() }
                .onChange(of: isGone) { _, isGone in
                    if isGone { onGone() }
                }
                .onAppear {
                    if isGone { onGone() }
                }
        } else {
            content
        }
    }
}

/// Runs an edit mode's save or delete and says whether the sheet should
/// close: yes on success, and yes when the entry turned out to be gone.
/// Any other failure keeps the sheet open with the user's edits, reported
/// as `failure` — or not at all when `failure` is nil, for a model that
/// shows its own message in the form.
@MainActor
func performEdit(_ write: () throws -> Void, failure: String?, reporter: ErrorReporter) -> Bool {
    do {
        try write()
        reporter.clear()
        return true
    } catch EditModeError.gone {
        return true
    } catch {
        if let failure { reporter.report(failure, error: error, category: AppLog.store) }
        return false
    }
}

public extension View {
    /// Reloads on every `revision` and calls `onGone` once the entry is gone.
    /// Does nothing when `isPresented` is false, so create mode is untouched.
    func editLifecycle(
        isPresented: Bool = true,
        revision: Int,
        isGone: Bool,
        reload: @escaping () -> Void,
        onGone: @escaping () -> Void
    ) -> some View {
        modifier(EditLifecycle(isPresented: isPresented, revision: revision, isGone: isGone, reload: reload, onGone: onGone))
    }

}

extension EditModel {
    /// Takes `fresh` into a field only while the user has not changed it from
    /// what was last loaded, so the other phone's edit lands in the fields
    /// left alone and never overwrites what is being typed.
    static func merge<Value: Equatable>(_ draft: inout Value, loaded: Value, fresh: Value) {
        if draft == loaded { draft = fresh }
    }

    /// " " is not a value: blank free text is stored as absent, the same as
    /// the create path does.
    static func trimmedToNil(_ text: String) -> String? {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }
}

/// A namespace for the helpers the edit models share.
enum EditModel {}
