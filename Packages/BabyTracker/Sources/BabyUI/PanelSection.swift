import SwiftUI
#if os(iOS)
import UIKit
#endif

/// A group of rows in one rounded card, with a hairline between rows and an
/// optional footer under the card — what a `Form` section drew, for content
/// that cannot be a `Form` (a fitted sheet measures a `Form` as zero high).
///
/// Every direct child is a row: padded, at least 44 pt tall, separated from
/// the next. A child that is not shown (an `if`) takes no row.
public struct PanelSection<Content: View, Footer: View>: View {
    private let content: Content
    private let footer: Footer

    public init(@ViewBuilder content: () -> Content, @ViewBuilder footer: () -> Footer) {
        self.content = content()
        self.footer = footer()
    }

    public var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Group(subviews: content) { rows in
                if !rows.isEmpty {
                    VStack(spacing: 0) {
                        ForEach(rows) { row in
                            if row.id != rows.first?.id {
                                Divider().padding(.leading, 16)
                            }
                            row
                                .padding(.horizontal, 16)
                                .padding(.vertical, 4)
                                .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                        }
                    }
                    .background(PanelSection<EmptyView, EmptyView>.cardFill,
                                in: .rect(cornerRadius: 24, style: .continuous))
                }
            }
            footer
                .font(.footnote)
                .foregroundStyle(Palette.secondaryText)
                .padding(.horizontal, 16)
        }
    }
}

public extension PanelSection where Footer == EmptyView {
    init(@ViewBuilder content: () -> Content) {
        self.init(content: content, footer: { EmptyView() })
    }
}

extension PanelSection where Content == EmptyView, Footer == EmptyView {
    /// The card behind the rows: the grouped-row fill a sheet's Form used.
    static var cardFill: Color {
        #if os(iOS)
        Color(uiColor: .tertiarySystemFill)
        #else
        Color.primary.opacity(0.08)
        #endif
    }
}
