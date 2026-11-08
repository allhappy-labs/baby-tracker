import SwiftUI

/// `CardSection` for a `List` styled with `.cardListStyle()`: the same header
/// and footer text roles, each row on `Surface.card`. `CardSection` is a plain
/// stack; a screen that needs swipe actions or reordering must be a `List`,
/// and this is the `List` `Section` with the same look.
public struct CardListSection<Content: View, Footer: View>: View {
    private let title: String?
    private let content: Content
    private let footer: Footer

    public init(_ title: String? = nil, @ViewBuilder content: () -> Content, @ViewBuilder footer: () -> Footer) {
        self.title = title
        self.content = content()
        self.footer = footer()
    }

    public var body: some View {
        Section {
            content
                .listRowBackground(Surface.card)
                // A List starts each hairline at the row's first text, so a
                // centred title (a `DestructiveButton`) pulled it in by half
                // the row. Every row's hairline spans the row instead.
                .alignmentGuide(.listRowSeparatorLeading) { _ in 0 }
                .alignmentGuide(.listRowSeparatorTrailing) { d in d.width }
        } header: {
            if let title {
                Text(title)
                    .textRole(.sectionLabel)
                    .textCase(nil)
            }
        } footer: {
            footer.textRole(.caption)
        }
    }
}

public extension CardListSection where Footer == EmptyView {
    init(_ title: String? = nil, @ViewBuilder content: () -> Content) {
        self.init(title, content: content, footer: { EmptyView() })
    }
}

/// A section of a card `List` that is a button (or a few) on the ground
/// rather than in a card: no card background, no separator, full card width.
public struct CardListButtonSection<Content: View, Footer: View>: View {
    private let content: Content
    private let footer: Footer

    public init(@ViewBuilder content: () -> Content, @ViewBuilder footer: () -> Footer) {
        self.content = content()
        self.footer = footer()
    }

    public var body: some View {
        Section {
            content
                .listRowBackground(Color.clear)
                .listRowSeparator(.hidden)
                // The card edges, like a sheet footer's primary.
                .listRowInsets(EdgeInsets(top: Spacing.xs, leading: 0, bottom: Spacing.xs, trailing: 0))
        } footer: {
            footer.textRole(.caption)
        }
    }
}

public extension CardListButtonSection where Footer == EmptyView {
    init(@ViewBuilder content: () -> Content) {
        self.init(content: content, footer: { EmptyView() })
    }
}

/// An action in a card: its title in the tint (the accent by default),
/// leading, wrapping by the word, the whole row tappable. Destructive actions
/// use `DestructiveButton` instead.
public struct ActionRow: View {
    private let title: String
    private let identifier: String
    private let systemImage: String?
    private let tint: Color
    private let action: () -> Void
    @Environment(\.isEnabled) private var isEnabled

    public init(
        _ title: String,
        identifier: String,
        systemImage: String? = nil,
        tint: Color = Palette.feed,
        action: @escaping () -> Void
    ) {
        self.title = title
        self.identifier = identifier
        self.systemImage = systemImage
        self.tint = tint
        self.action = action
    }

    public var body: some View {
        Button(action: action) {
            HStack(spacing: Spacing.m) {
                if let systemImage {
                    Image(systemName: systemImage).accessibilityHidden(true)
                }
                Text(title).fixedSize(horizontal: false, vertical: true)
                Spacer(minLength: 0)
            }
            // Inside the label: a button's hit region is its label's content,
            // and the Spacer is the middle of the row.
            .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
            .contentShape(Rectangle())
        }
        // Dimmed tint, not system grey: a disabled action must not read as
        // the placeholder text of the field above it.
        .foregroundStyle(tint)
        .opacity(PrimaryButtonStyle.opacity(isEnabled: isEnabled))
        .accessibilityIdentifier(identifier)
    }
}
