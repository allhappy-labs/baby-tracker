import SwiftUI
import BabyCore

/// A root screen: the ground, a header row (the baby switcher or a plain title
/// with trailing controls) and the content. With `scrolls` the scaffold owns a
/// `ScrollView`; without it the content fills the space below the header and
/// owns its scrolling (History's `List`, Ask's conversation). With
/// `hostingHeader` the content owns the header row too, and places it inside
/// its own scroll view so the header scrolls away with it (Home).
public struct ScreenScaffold<Trailing: View, Content: View>: View {
    public typealias Header = ScreenHeader

    private let headerRow: ScreenHeaderRow<Trailing>
    private let scrolls: Bool
    private let contentHostsHeader: Bool
    private let content: Content

    public init(
        header: Header,
        scrolls: Bool = true,
        @ViewBuilder trailing: () -> Trailing,
        @ViewBuilder content: () -> Content
    ) {
        self.headerRow = ScreenHeaderRow(header: header, trailing: trailing())
        self.scrolls = scrolls
        self.contentHostsHeader = false
        self.content = content()
    }

    /// The content gets the header row and owns its scrolling: it puts the
    /// row inside its scroll view, so the header scrolls with the content.
    public init(
        header: Header,
        @ViewBuilder trailing: () -> Trailing,
        @ViewBuilder hostingHeader content: (ScreenHeaderRow<Trailing>) -> Content
    ) {
        let row = ScreenHeaderRow(header: header, trailing: trailing())
        self.headerRow = row
        self.scrolls = false
        self.contentHostsHeader = true
        self.content = content(row)
    }

    public var body: some View {
        VStack(spacing: Spacing.m) {
            if !contentHostsHeader {
                headerRow
            }
            if scrolls {
                ScrollView {
                    VStack(spacing: Spacing.m) { content }
                        .padding(.horizontal, Spacing.l)
                }
            } else {
                content
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
        .background(Palette.ground.ignoresSafeArea())
        .toolbarBackground(.hidden)
    }
}

/// What a root screen's header shows: the baby switcher or a plain title.
public enum ScreenHeader {
    case baby(name: String, avatar: BabyAvatar, switcherID: String, headingID: String,
              onSwitch: (() -> Void)?)
    case title(String, identifier: String)
}

/// A root screen's header row: the header, then its trailing controls,
/// inset by the screen margin.
public struct ScreenHeaderRow<Trailing: View>: View {
    let header: ScreenHeader
    let trailing: Trailing

    public var body: some View {
        HStack(spacing: Spacing.s) {
            switch header {
            case let .baby(name, avatar, switcherID, headingID, onSwitch):
                BabySwitcherHeader(babyName: name, avatar: avatar, switcherID: switcherID,
                                   headingID: headingID, onSwitchBaby: onSwitch)
            case let .title(title, identifier):
                Text(title)
                    .textRole(.screenTitle)
                    .accessibilityAddTraits(.isHeader)
                    .accessibilityIdentifier(identifier)
            }
            Spacer(minLength: Spacing.s)
            HStack(spacing: Spacing.s) { trailing }
        }
        .padding(.horizontal, Spacing.l)
    }
}

public extension ScreenScaffold where Trailing == EmptyView {
    init(header: Header, scrolls: Bool = true, @ViewBuilder content: () -> Content) {
        self.init(header: header, scrolls: scrolls, trailing: { EmptyView() }, content: content)
    }
}
