import SwiftUI
import BabyCore
import BabyUI

enum HomeButtonsCopy {
    static let rowTitle = String(localized: "Home buttons", bundle: .module)
    static let bigButton = String(localized: "Big button", bundle: .module)
    static let tiles = String(localized: "Tiles", bundle: .module)
    static let inMore = String(localized: "In More (···)", bundle: .module)
    static let breastFooter = String(localized: "Shown as Left and Right.", bundle: .module)
    static let reset = String(localized: "Reset to default", bundle: .module)
    static let moveUp = String(localized: "Move up", bundle: .module)
    static let moveDown = String(localized: "Move down", bundle: .module)
    static let done = String(localized: "Done", bundle: .module)

    /// "Home buttons · Mira": the order belongs to one baby.
    static func title(_ babyName: String) -> String { "\(rowTitle) · \(babyName)" }

    /// The heading drawn above a row. They belong to positions, not to
    /// actions, so they stay put while rows move under them.
    static func heading(at index: Int) -> String? {
        switch index {
        case 0: bigButton
        case 1: tiles
        case HomeLayout.tileCount + 1: inMore
        default: nil
        }
    }
}

/// One list, dragged into order: the first action is Home's big button, the
/// next three its tiles, the rest live behind ···. Every change is saved as
/// it is made.
public struct HomeButtonsView: View {
    private let settings: SettingsViewModel
    private let babyID: UUID
    private let babyName: String

    public init(settings: SettingsViewModel, babyID: UUID, babyName: String) {
        self.settings = settings
        self.babyID = babyID
        self.babyName = babyName
    }

    private var order: [HomeActionKind] { settings.homeOrder(for: babyID) }

    public var body: some View {
        List {
            Section {
                ForEach(Array(order.enumerated()), id: \.element) { index, kind in
                    row(kind, at: index)
                }
                .onMove { from, to in
                    var next = order
                    next.move(fromOffsets: from, toOffset: to)
                    settings.setHomeOrder(next, for: babyID)
                }
                .listRowBackground(Surface.card)
            }
            Section {
                Button(HomeButtonsCopy.reset) { settings.resetHomeOrder(for: babyID) }
                    .disabled(order == HomeActionKind.defaultOrder)
                    .accessibilityIdentifier("homeButtons.reset")
                    .listRowBackground(Surface.card)
            }
        }
        // A `List`, not cards, because the drag to reorder is the List's;
        // drawn like the app's cards, as History's is.
        .cardListStyle()
        .background(Palette.ground.ignoresSafeArea())
        .toolbarBackground(.hidden)
        // Always reorderable: there is nothing else to do on this screen.
        #if os(iOS)
        .environment(\.editMode, .constant(.active))
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .navigationTitle(HomeButtonsCopy.title(babyName))
    }

    private func row(_ kind: HomeActionKind, at index: Int) -> some View {
        VStack(alignment: .leading, spacing: Spacing.xs) {
            if let heading = HomeButtonsCopy.heading(at: index) {
                Text(heading)
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(Palette.secondaryText)
            }
            HStack(spacing: Spacing.m) {
                BabyIconView(kind.icon, size: 22)
                    .foregroundStyle(kind.color)
                Text(kind.title)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .frame(minHeight: 44)
            // Breast is the one action the big button draws as two.
            if index == 0, kind == .breast {
                Text(HomeButtonsCopy.breastFooter)
                    .font(.footnote)
                    .foregroundStyle(Palette.secondaryText)
                    .accessibilityIdentifier("homeButtons.breastNote")
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("homeButtons.row.\(kind.rawValue)")
        .accessibilityAction(named: HomeButtonsCopy.moveUp) {
            settings.moveHomeAction(at: index, by: -1, for: babyID)
        }
        .accessibilityAction(named: HomeButtonsCopy.moveDown) {
            settings.moveHomeAction(at: index, by: 1, for: babyID)
        }
    }
}

public extension HomeButtonsView {
    /// The title of the Done button the app adds when it presents this
    /// screen as a sheet, from the ··· sheet's last row.
    static var doneTitle: String { HomeButtonsCopy.done }
}
