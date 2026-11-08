import SwiftUI
import BabyCore
import BabyUI

/// The one shell every log and edit panel shares, on the kit's
/// `SheetScaffold`: the close ✕ and the activity's icon on top, its fields in
/// grouped cards, Save full width at the bottom and, in edit mode, Delete on
/// its own row above it. Presented with `fittedSheet`, the sheet is exactly as
/// tall as this and follows it as Details opens or a message appears; past the
/// screen the fields scroll and the header and footer stay put.
///
/// No `Form` and no navigation bar: a fitted sheet measures either as zero.
public struct EntryPanel<Content: View>: View {
    private let title: String
    private let icon: BabyIcon
    private let tint: Color
    private let canSave: Bool
    private let identifierPrefix: String
    private let onCancel: (() -> Void)?
    private let cancelTitle: String
    private let onSave: (() -> Void)?
    private let onDelete: (() -> Void)?
    private let accessory: AnyView?
    private let logAction: SheetAction?
    private let content: Content
    /// The one time wheel open in the panel.
    @State private var openTimeWheel: String?
    @State private var isConfirmingDelete = false

    /// No `onSave` means no footer at all: the Diaper sheet's new mode, where
    /// a choice saves. `accessory` sits at the header's right (Supplement's
    /// add-a-name). `logAction` is the primary of a panel whose new mode logs
    /// rather than saves (Pump's Log), used only when there is no `onSave`.
    /// `cancelTitle` is the ✕'s accessibility label: Back where
    /// the panel was picked from Past entry's list and returns to it.
    public init(title: String, icon: BabyIcon, tint: Color, canSave: Bool, identifierPrefix: String,
                onCancel: (() -> Void)?, cancelTitle: String = EditChromeCopy.cancel, onSave: (() -> Void)?, onDelete: (() -> Void)?,
                accessory: AnyView? = nil, logAction: SheetAction? = nil, @ViewBuilder content: () -> Content) {
        self.title = title
        self.icon = icon
        self.tint = tint
        self.canSave = canSave
        self.identifierPrefix = identifierPrefix
        self.onCancel = onCancel
        self.cancelTitle = cancelTitle
        self.onSave = onSave
        self.onDelete = onDelete
        self.accessory = accessory
        self.logAction = logAction
        self.content = content()
    }

    private var actions: EditActions {
        // Delete only asks; the confirmation does the deleting.
        EditActions(identifierPrefix: identifierPrefix, canSave: canSave, onSave: onSave,
                    onDelete: onDelete.map { _ in { isConfirmingDelete = true } })
    }

    public var body: some View {
        SheetScaffold(closeTitle: cancelTitle, onClose: onCancel, primary: actions.primary ?? logAction, destructive: actions.destructive) {
            PanelTitle(icon: icon, tint: tint, label: title)
        } accessory: {
            if let accessory { accessory }
        } content: {
            content
        }
        .environment(\.openTimeWheel, $openTimeWheel)
        // System controls (Picker, Stepper, Toggle, DatePicker) in the
        // tracker's colour, not the system blue.
        .tint(tint)
        // The dialog is never shown without the button that asks for it.
        .confirmDelete(isPresented: $isConfirmingDelete, title: EditChromeCopy.deleteConfirmTitle,
                       deleteTitle: EditChromeCopy.delete, keepTitle: EditChromeCopy.keep,
                       identifierPrefix: identifierPrefix, onDelete: onDelete ?? {})
    }
}

/// The panel's title: the tracker's icon instead of a word that repeats it.
/// The icon is hidden from VoiceOver; the panel still needs a name, and a
/// heading to start from.
struct PanelTitle: View {
    let icon: BabyIcon
    let tint: Color
    let label: String

    var body: some View {
        BabyIconView(icon, size: 24)
            .foregroundStyle(tint)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(label)
            .accessibilityAddTraits(.isHeader)
    }
}

/// Details: collapsed unless it holds something. Collapsed, it is one row —
/// the sheet follows it open and shut.
public struct DetailsSection<Fields: View>: View {
    @Binding private var isExpanded: Bool
    private let identifier: String
    private let fields: Fields

    public init(isExpanded: Binding<Bool>, identifier: String, @ViewBuilder fields: () -> Fields) {
        _isExpanded = isExpanded
        self.identifier = identifier
        self.fields = fields()
    }

    public var body: some View {
        CardSection {
            Button {
                withAnimation(.snappy) { isExpanded.toggle() }
            } label: {
                ListRow(EditChromeCopy.details, accessory: .expand(isExpanded: isExpanded))
                    .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                    .contentShape(.rect)
            }
            .buttonStyle(.plain)
            .accessibilityValue(isExpanded ? EditChromeCopy.expanded : EditChromeCopy.collapsed)
            .accessibilityIdentifier(identifier)

            if isExpanded {
                fields
            }
        }
    }
}

/// One field under Details, the same on every panel: its name on the left,
/// what was typed on the right, "Optional" until then. At the accessibility
/// sizes the name goes above the field, where both no longer fit a row.
public struct DetailFieldRow: View {
    private let title: String
    @Binding private var text: String
    private let prompt: String
    private let identifier: String
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(_ title: String, text: Binding<String>, prompt: String = EditChromeCopy.optional,
                identifier: String) {
        self.title = title
        _text = text
        self.prompt = prompt
        self.identifier = identifier
    }

    public var body: some View {
        let label = Text(title).foregroundStyle(Palette.primaryText)
        let field = TextField(title, text: $text, prompt: Text(prompt))
            .foregroundStyle(Palette.primaryText)
            .accessibilityLabel(title)
            .accessibilityIdentifier(identifier)
        if typeSize.isAccessibilitySize {
            VStack(alignment: .leading, spacing: 4) {
                label
                field
            }
            .padding(.vertical, 6)
        } else {
            HStack(spacing: 12) {
                // The name keeps its width; the field takes the rest and
                // scrolls its own text.
                label.fixedSize().layoutPriority(1)
                field.multilineTextAlignment(.trailing)
            }
            .frame(minHeight: 44)
        }
    }
}
