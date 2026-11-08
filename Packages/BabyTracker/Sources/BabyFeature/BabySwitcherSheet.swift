import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public struct BabySwitcherSheet: View {
    let store: BabyStore
    let selection: SelectedBabyStore
    let onDone: () -> Void

    @State private var babies: [Baby] = []
    @State private var isAdding = false

    public init(store: BabyStore, selection: SelectedBabyStore, onDone: @escaping () -> Void) {
        self.store = store
        self.selection = selection
        self.onDone = onDone
    }

    public var body: some View {
        SheetScaffold(
            L10n.tr("Babies"),
            closeTitle: L10n.tr("Done"),
            onClose: onDone
        ) {
            CardSection {
                ForEach(babies, id: \.id) { baby in
                    Button {
                        selection.select(baby.id)
                        onDone()
                    } label: {
                        HStack(spacing: Spacing.m) {
                            BabyAvatarView(BabyAvatar(iconID: baby.iconID), size: 28)
                            Text(baby.name)
                                .foregroundStyle(Palette.primaryText)
                                .fixedSize(horizontal: false, vertical: true)
                            Spacer(minLength: Spacing.s)
                            if baby.id == selection.selectedID {
                                Image(systemName: "checkmark")
                                    .foregroundStyle(Palette.feed)
                                    .accessibilityHidden(true)
                            }
                        }
                        .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityAddTraits(baby.id == selection.selectedID ? .isSelected : [])
                }

                Button { isAdding = true } label: {
                    Text(L10n.tr("Add baby"))
                        .foregroundStyle(Palette.feed)
                        .fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
            }
        }
        .tint(Palette.feed)
        .sheet(isPresented: $isAdding) {
            AddBabySheet(
                store: store,
                onAdded: { baby in
                    selection.select(baby.id)
                    isAdding = false
                    onDone()
                },
                onCancel: { isAdding = false }
            )
        }
        .task { babies = (try? store.babies()) ?? [] }
    }
}
