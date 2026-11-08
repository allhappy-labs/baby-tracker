import SwiftUI
import BabyUI

/// A screen pushed from Settings: its cards in one scrolling column on the
/// ground, under the navigation bar's title.
struct SettingsPage<Content: View>: View {
    private let content: Content

    init(@ViewBuilder content: () -> Content) {
        self.content = content()
    }

    var body: some View {
        ScrollView {
            VStack(spacing: Spacing.m) { content }
                .padding(.horizontal, Spacing.l)
                .padding(.vertical, Spacing.m)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .settingsPageChrome()
    }
}

/// A pushed Settings screen that needs a `List` (swipe actions): the same
/// cards and ground, with `CardListSection`s for sections.
struct SettingsListPage<Content: View>: View {
    private let content: Content

    init(@ViewBuilder content: () -> Content) {
        self.content = content()
    }

    var body: some View {
        List { content }
            .cardListStyle()
            .settingsPageChrome()
    }
}

private extension View {
    /// The ground behind, no bar background, and an inline title, as every
    /// screen pushed from Settings had while it was a `Form`.
    func settingsPageChrome() -> some View {
        self
            .background(Palette.ground.ignoresSafeArea())
            .toolbarBackground(.hidden)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
    }
}
