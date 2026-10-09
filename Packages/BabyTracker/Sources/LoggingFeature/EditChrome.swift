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
}

/// An edited entry's time, with its date: an entry is often corrected the day
/// after, and a feed that started before midnight belongs to the day before
/// (ruling H-R7). Never later than `latest`, which callers set to now.
public struct EditTimeRow: View {
    private let label: String
    @Binding private var date: Date
    private let latest: Date
    private let identifier: String
    private let stacksAtAccessibilitySizes: Bool

    /// `stacksAtAccessibilitySizes` puts the label over the date and time
    /// at the accessibility sizes, for a sheet that lays the row out itself
    /// (the Diaper sheet's VStack) rather than in a Form row.
    public init(_ label: String = EditChromeCopy.timeLabel, date: Binding<Date>, latest: Date, identifier: String,
                stacksAtAccessibilitySizes: Bool = false) {
        self.label = label
        _date = date
        self.latest = latest
        self.identifier = identifier
        self.stacksAtAccessibilitySizes = stacksAtAccessibilitySizes
    }

    @Environment(\.dynamicTypeSize) private var typeSize

    public var body: some View {
        if stacksAtAccessibilitySizes, typeSize.isAccessibilitySize {
            // The label over the date and the time: beside them, at these
            // sizes, it was squeezed to "D…".
            VStack(alignment: .leading, spacing: 6) {
                Text(label)
                    .accessibilityHidden(true)
                picker.labelsHidden()
            }
        } else {
            picker
        }
    }

    private var picker: some View {
        DatePicker(label, selection: $date, in: ...latest, displayedComponents: [.date, .hourAndMinute])
            .frame(minHeight: 44)
            .accessibilityIdentifier(identifier)
    }
}

/// Save and Delete, pinned under an edit mode's content. Save is the sheet's
/// one primary action; Delete sits under it in the destructive colour and
/// only asks for the confirmation — `deleteConfirmation` does the asking.
public struct EditFooter: View {
    private let canSave: Bool
    private let identifierPrefix: String
    private let onSave: () -> Void
    private let onDelete: () -> Void

    /// What the footer adds to a sheet's stated height: Save at 56, Delete
    /// at 44, and the padding around them.
    public static let height: CGFloat = 136

    public init(canSave: Bool, identifierPrefix: String, onSave: @escaping () -> Void, onDelete: @escaping () -> Void) {
        self.canSave = canSave
        self.identifierPrefix = identifierPrefix
        self.onSave = onSave
        self.onDelete = onDelete
    }

    public var body: some View {
        VStack(spacing: 8) {
            Button(action: onSave) {
                Text(EditChromeCopy.save)
                    .font(.headline)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .buttonStyle(.primaryFill(minHeight: 56))
            .disabled(!canSave)
            .opacity(canSave ? 1 : 0.5)
            .accessibilityIdentifier(identifierPrefix + ".save")

            Button(role: .destructive, action: onDelete) {
                Text(EditChromeCopy.delete)
                    .font(.body.weight(.semibold))
                    .foregroundStyle(Palette.destructive)
                    .frame(maxWidth: .infinity, minHeight: 44)
                    .contentShape(.rect)
            }
            .buttonStyle(.plain)
            .accessibilityIdentifier(identifierPrefix + ".delete")
        }
        .padding(.horizontal, 20)
        .padding(.top, 12)
        .padding(.bottom, 8)
        .background(.bar)
    }
}

private struct DeleteConfirmation: ViewModifier {
    @Binding var isPresented: Bool
    let title: String
    let identifierPrefix: String
    let onDelete: () -> Void

    func body(content: Content) -> some View {
        content.confirmationDialog(title, isPresented: $isPresented, titleVisibility: .visible) {
            // Its own identifier: while the dialog is up, the footer's Delete
            // carries the same label, and a query by label finds both.
            Button(EditChromeCopy.delete, role: .destructive, action: onDelete)
                .accessibilityIdentifier(identifierPrefix + ".confirmDelete")
            Button(EditChromeCopy.keep, role: .cancel) {}
                .accessibilityIdentifier(identifierPrefix + ".keep")
        }
    }
}

/// The footer and the confirmation behind its Delete, applied only in edit
/// mode so a create sheet's layout is untouched.
private struct EditFooterModifier: ViewModifier {
    let isPresented: Bool
    let canSave: Bool
    let identifierPrefix: String
    let onSave: () -> Void
    let onDelete: () -> Void
    @State private var isConfirmingDelete = false

    func body(content: Content) -> some View {
        if isPresented {
            content
                .safeAreaInset(edge: .bottom, spacing: 0) {
                    EditFooter(canSave: canSave, identifierPrefix: identifierPrefix, onSave: onSave) {
                        isConfirmingDelete = true
                    }
                }
                .deleteConfirmation(isPresented: $isConfirmingDelete, identifierPrefix: identifierPrefix,
                                    onDelete: onDelete)
        } else {
            content
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

/// Edit mode's title: the tracker's icon instead of a word that repeats it.
/// The entry's time, the row that identifies it, is the first row below.
private struct EditTitleModifier: ViewModifier {
    let isPresented: Bool
    let icon: BabyIcon
    let tint: Color
    let label: String

    func body(content: Content) -> some View {
        if isPresented {
            content.toolbar {
                ToolbarItem(placement: .principal) {
                    ZStack { BabyIconView(icon, size: 24).foregroundStyle(tint) }
                        // The icon itself is hidden from VoiceOver; the sheet
                        // still needs a name, and a heading to start from.
                        .accessibilityElement(children: .ignore)
                        .accessibilityLabel(label)
                        .accessibilityAddTraits(.isHeader)
                }
            }
        } else {
            content
        }
    }
}

public extension View {
    /// Asks before an edit mode deletes its entry. A deleted entry is a
    /// tombstone that syncs to the other phone, and there is no undo.
    /// Its buttons are `<identifierPrefix>.confirmDelete` and `.keep`.
    /// `title` names what goes: "Delete this entry?" unless told otherwise.
    func deleteConfirmation(isPresented: Binding<Bool>, title: String = EditChromeCopy.deleteConfirmTitle,
                            identifierPrefix: String, onDelete: @escaping () -> Void) -> some View {
        modifier(DeleteConfirmation(isPresented: isPresented, title: title, identifierPrefix: identifierPrefix,
                                    onDelete: onDelete))
    }

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

    /// The edit-mode footer, pinned under the content so a Form scrolls
    /// behind it rather than pushing it off the sheet, with Delete's
    /// confirmation. Does nothing when `isPresented` is false.
    func editFooter(
        isPresented: Bool = true,
        canSave: Bool,
        identifierPrefix: String,
        onSave: @escaping () -> Void,
        onDelete: @escaping () -> Void
    ) -> some View {
        modifier(EditFooterModifier(isPresented: isPresented, canSave: canSave, identifierPrefix: identifierPrefix,
                                    onSave: onSave, onDelete: onDelete))
    }

    /// Puts the tracker icon in the navigation bar in edit mode. `label` is
    /// what VoiceOver reads for it: the sheet's own title.
    func editTitle(isPresented: Bool = true, icon: BabyIcon, tint: Color, label: String) -> some View {
        modifier(EditTitleModifier(isPresented: isPresented, icon: icon, tint: tint, label: label))
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
