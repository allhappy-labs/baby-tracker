#if DEBUG
import SwiftUI
import BabyCore

/// Every kit component and state on the ground, for review in Xcode previews.
struct KitGallery: View {
    var body: some View {
        ScrollView { KitGalleryContent() }
            .background(Palette.ground)
            .preferredColorScheme(.dark)
    }
}

struct KitGalleryContent: View {
    var body: some View {
        VStack(alignment: .leading, spacing: Spacing.xl) {
            typography
            cards
            chips
            buttons
            screenParts
            sheets
        }
        .padding(Spacing.l)
    }

    private func block<Content: View>(_ name: String, @ViewBuilder _ content: () -> Content) -> some View {
        VStack(alignment: .leading, spacing: Spacing.s) {
            Text(name).textRole(.sectionLabel)
            content()
        }
    }

    private var typography: some View {
        block("Text roles") {
            ForEach(TextRole.allCases, id: \.self) { role in
                Text("\(String(describing: role)) — Грудне молоко").textRole(role)
            }
        }
    }

    private var cards: some View {
        block("Card, CardSection, ListRow") {
            Card { Text("A card").textRole(.body) }
            CardSection("Time") {
                ListRow("Started", value: "9:41", icon: .feed, tint: Palette.feed)
                ListRow("Ended", value: "10:05", icon: .sleep, tint: Palette.sleep)
                ListRow("Details", subtitle: "Left breast", accessory: .disclosure)
                ListRow("Expanded", accessory: .expand(isExpanded: true))
                ListRow("Collapsed", accessory: .expand(isExpanded: false))
            } footer: {
                Text("Saved to the shared family log.")
            }
        }
    }

    private var chips: some View {
        block("Chip, selectable chip") {
            HStack {
                Chip("Left", icon: .breastLeft, tint: Palette.feed)
                Chip("Small", tint: Palette.sleep, size: .small)
                Chip("Removable", tint: Palette.diaper, onRemove: {}, removeLabel: "Remove")
            }
            HStack {
                Button("30 ml") {}.buttonStyle(.selectableChip(Palette.pump, isSelected: true))
                Button("60 ml") {}.buttonStyle(.selectableChip(Palette.pump, isSelected: false))
            }
        }
    }

    private var buttons: some View {
        block("Buttons") {
            Button("Save") {}.buttonStyle(.primary)
            Button("Save (disabled)") {}.buttonStyle(.primary).disabled(true)
            Button("Cancel") {}.buttonStyle(.secondary)
            DestructiveButton("Delete", identifier: "gallery.delete") {}
            HStack {
                CircleIconButton(systemImage: "gearshape", label: "Settings", identifier: "gallery.settings") {}
                CircleIconButton(icon: .more, label: "More", identifier: "gallery.more", showsBadge: true) {}
                CircleIconButton(systemImage: "plus", label: "Add", identifier: "gallery.add", isProminent: true) {}
            }
            HStack {
                Button { } label: { Label("Left", systemImage: "drop") }
                    .buttonStyle(.tile(Palette.feed))
                Button { } label: { Label("Right", systemImage: "drop") }
                    .buttonStyle(.tile(Palette.feed, isActive: true))
            }
        }
    }

    private var screenParts: some View {
        block("EmptyState, InlineError, PartsRow") {
            Card {
                EmptyState(icon: .feed, title: "No feedings yet", message: "Log the first one from Home.")
            }
            InlineError("Could not save. Try again in a moment.")
            InlineError("No glyph", systemImage: nil)
            let items = [
                PartsRow.Item(icon: .breastLeft, tint: Palette.feed, text: "9:02"),
                PartsRow.Item(icon: .breastRight, tint: Palette.feed, text: "4:10"),
                PartsRow.Item(icon: .bottle, tint: Palette.feed, text: "2:00"),
            ]
            PartsRow(items)
            PartsRow(items, size: .widget, earlierCount: 3, earlierBadge: { "+\($0)" },
                     earlierLabel: { "\($0) earlier parts" })
            PartsRow(items).frame(width: 110, alignment: .leading)
        }
    }

    private var sheets: some View {
        block("ConfirmSheet") {
            ConfirmSheet(systemImage: "trash", tint: Palette.destructive, title: "Delete this feeding?",
                         message: "It is removed for everyone in the family.",
                         confirm: SheetAction("Delete", identifier: "gallery.confirm") {},
                         isDestructive: true,
                         cancel: SheetAction("Keep", identifier: "gallery.cancel") {})
            .frame(height: 320)
        }
    }
}

#Preview("Kit") {
    KitGallery()
}

#Preview("Kit – AX5") {
    KitGallery().environment(\.dynamicTypeSize, .accessibility5)
}

#Preview("ScreenScaffold") {
    ScreenScaffold(header: .baby(name: "Maya", avatar: .girl1, switcherID: "gallery.switcher",
                                 headingID: "gallery.heading", onSwitch: {})) {
        CircleIconButton(systemImage: "gearshape", label: "Settings", identifier: "gallery.settings") {}
    } content: {
        Card { Text("Content").textRole(.body) }
    }
}
#endif
