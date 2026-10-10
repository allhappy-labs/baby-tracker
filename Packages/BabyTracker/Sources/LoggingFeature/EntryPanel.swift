import SwiftUI
import BabyCore
import BabyUI

/// The one shell every log and edit panel shares: Cancel and the activity's
/// icon on top, its fields in grouped cards, Save (and in edit mode Delete)
/// at the bottom. Presented with `fittedSheet`, the sheet is exactly as tall
/// as this and follows it as Details opens or a message appears; past the
/// screen the fields scroll and the header and footer stay put.
///
/// No `Form` and no navigation bar: a fitted sheet measures either as zero.
/// The fields use `PanelSection` where a `Form` would use `Section`.
public struct EntryPanel<Content: View>: View {
    private let title: String
    private let icon: BabyIcon
    private let tint: Color
    private let canSave: Bool
    private let identifierPrefix: String
    private let onCancel: (() -> Void)?
    private let onSave: (() -> Void)?
    private let onDelete: (() -> Void)?
    private let accessory: AnyView?
    private let content: Content

    /// No `onSave` means no footer at all: the Diaper sheet's new mode, where
    /// a choice saves. `accessory` sits at the header's right (Supplement's
    /// add-a-name).
    public init(title: String, icon: BabyIcon, tint: Color, canSave: Bool, identifierPrefix: String,
                onCancel: (() -> Void)?, onSave: (() -> Void)?, onDelete: (() -> Void)?,
                accessory: AnyView? = nil, @ViewBuilder content: () -> Content) {
        self.title = title
        self.icon = icon
        self.tint = tint
        self.canSave = canSave
        self.identifierPrefix = identifierPrefix
        self.onCancel = onCancel
        self.onSave = onSave
        self.onDelete = onDelete
        self.accessory = accessory
        self.content = content()
    }

    public var body: some View {
        VStack(spacing: 0) {
            SheetHeader(cancelTitle: EditChromeCopy.cancel, onCancel: onCancel) {
                PanelTitle(icon: icon, tint: tint, label: title)
            } trailing: {
                if let accessory { accessory }
            }

            ScrollView {
                VStack(spacing: 12) { content }
                    .padding(.horizontal, 16)
                    .padding(.vertical, 12)
            }
            .scrollBounceBehavior(.basedOnSize)
            .editFooter(isPresented: onSave != nil, canSave: canSave, identifierPrefix: identifierPrefix,
                        onSave: onSave ?? {}, onDelete: onDelete)
        }
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
        PanelSection {
            Button {
                withAnimation(.snappy) { isExpanded.toggle() }
            } label: {
                HStack {
                    Text(EditChromeCopy.details)
                        .foregroundStyle(Palette.primaryText)
                    Spacer(minLength: 0)
                    Image(systemName: "chevron.right")
                        .font(.footnote.weight(.semibold))
                        .foregroundStyle(Palette.secondaryText)
                        .rotationEffect(.degrees(isExpanded ? 90 : 0))
                        .accessibilityHidden(true)
                }
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
