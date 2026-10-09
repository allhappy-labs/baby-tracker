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
        NavigationStack {
            List {
                ForEach(babies, id: \.id) { baby in
                    Button {
                        selection.select(baby.id)
                        onDone()
                    } label: {
                        HStack {
                            BabyAvatarView(BabyAvatar(iconID: baby.iconID), size: 28)
                            Text(baby.name)
                            Spacer()
                            if baby.id == selection.selectedID {
                                Image(systemName: "checkmark")
                                    .foregroundStyle(Palette.feed)
                            }
                        }
                        .frame(minHeight: 44)
                    }
                }

                Button(L10n.tr("Add baby")) { isAdding = true }
                    .frame(minHeight: 44)
            }
            .navigationTitle(L10n.tr("Babies"))
            #if os(iOS)
            // Inline title display mode is a UIKit navigation-bar concept.
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button(L10n.tr("Done"), action: onDone)
                }
            }
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
        }
        .task { babies = (try? store.babies()) ?? [] }
    }
}
