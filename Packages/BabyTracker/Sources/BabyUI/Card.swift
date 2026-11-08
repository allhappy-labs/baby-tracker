import SwiftUI

/// A padded rounded container on `Surface.card`.
public struct Card<Content: View>: View {
    private let padding: CGFloat
    private let content: Content

    public init(padding: CGFloat = Spacing.m, @ViewBuilder content: () -> Content) {
        self.padding = padding
        self.content = content()
    }

    public var body: some View {
        content
            .padding(padding)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Surface.card, in: RoundedRectangle(cornerRadius: Radius.card, style: .continuous))
    }
}

/// The chrome every row of a card shares: an optional hairline above it,
/// `Spacing.xs` above and below, at least 44 pt tall. Internal; rows inside a
/// `Card` get it through `.cardRow()`.
private struct CardRowChrome: ViewModifier {
    let showsSeparator: Bool
    let separatorInset: CGFloat
    let horizontalPadding: CGFloat
    @Environment(\.displayScale) private var displayScale

    func body(content: Content) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            if showsSeparator {
                // A device hairline: one pixel, not one point.
                Surface.separator
                    .frame(height: 1 / displayScale)
                    .padding(.leading, separatorInset)
            }
            content
                .padding(.horizontal, horizontalPadding)
                .padding(.vertical, Spacing.xs)
                .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
        }
    }
}

public extension View {
    /// A row inside a `Card` that also holds other content (a chart, say):
    /// a hairline above it, padded, at least 44 pt tall, the whole row
    /// tappable. Apply it to the row's content, inside any accessibility
    /// element the row makes: from outside, the element keeps its content's
    /// height and fails the 44 pt hit-region audit.
    func cardRow() -> some View {
        modifier(CardRowChrome(showsSeparator: true, separatorInset: 0, horizontalPadding: 0))
            .contentShape(Rectangle())
    }
}

/// A group of rows in one rounded card, with a hairline between rows, an
/// optional header above and footer below. Every direct child is a row:
/// padded, at least 44 pt tall, separated from the next. A child that is not
/// shown (an `if`) takes no row.
public struct CardSection<Header: View, Content: View, Footer: View>: View {
    private let header: Header
    private let content: Content
    private let footer: Footer

    public init(
        @ViewBuilder header: () -> Header,
        @ViewBuilder content: () -> Content,
        @ViewBuilder footer: () -> Footer
    ) {
        self.header = header()
        self.content = content()
        self.footer = footer()
    }

    public var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            header
                .textRole(.sectionLabel)
                .padding(.horizontal, Spacing.l)
            Group(subviews: content) { rows in
                if !rows.isEmpty {
                    VStack(spacing: 0) {
                        ForEach(rows) { row in
                            row.modifier(CardRowChrome(showsSeparator: row.id != rows.first?.id,
                                                       separatorInset: Spacing.l,
                                                       horizontalPadding: Spacing.l))
                        }
                    }
                    .background(Surface.card,
                                in: RoundedRectangle(cornerRadius: Radius.card, style: .continuous))
                }
            }
            footer
                .textRole(.caption)
                .padding(.horizontal, Spacing.l)
        }
    }
}

public extension CardSection where Header == EmptyView, Footer == EmptyView {
    init(@ViewBuilder content: () -> Content) {
        self.init(header: { EmptyView() }, content: content, footer: { EmptyView() })
    }
}

public extension CardSection where Header == EmptyView {
    init(@ViewBuilder content: () -> Content, @ViewBuilder footer: () -> Footer) {
        self.init(header: { EmptyView() }, content: content, footer: footer)
    }
}

public extension CardSection where Header == Text, Footer == EmptyView {
    init(_ title: String, @ViewBuilder content: () -> Content) {
        self.init(header: { Text(title) }, content: content, footer: { EmptyView() })
    }
}

public extension CardSection where Header == Text {
    init(_ title: String, @ViewBuilder content: () -> Content, @ViewBuilder footer: () -> Footer) {
        self.init(header: { Text(title) }, content: content, footer: footer)
    }
}

public extension View {
    /// A `List` drawn as the app's cards: inset grouped, no system
    /// background (the screen's ground shows through), `Spacing.m` between
    /// sections. For screens that need what only a `List` gives — swipe
    /// actions, drag to reorder. Rows set `.listRowBackground(Surface.card)`.
    func cardListStyle() -> some View {
        #if os(iOS)
        self
            .scrollContentBackground(.hidden)
            .listStyle(.insetGrouped)
            .listSectionSpacing(Spacing.m)
        #else
        self.scrollContentBackground(.hidden)
        #endif
    }
}
