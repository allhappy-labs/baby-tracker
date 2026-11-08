import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// Meds from the dock: the saved meds as chips, and "+" for a new one.
public struct MedsSheet: View {
    @State private var model: MoreSheetModel
    private let onSwitchTo: (LogSheet) -> Void
    private let onDone: () -> Void

    public init(store: BabyStore, babyID: UUID, onSwitchTo: @escaping (LogSheet) -> Void, onDone: @escaping () -> Void) {
        _model = State(initialValue: MoreSheetModel(store: store, babyID: babyID))
        self.onSwitchTo = onSwitchTo
        self.onDone = onDone
    }

    /// Presented with `fittedSheet`, so the title and Cancel are drawn here.
    public var body: some View {
        SheetScaffold(closeTitle: MoreSheetCopy.cancel, onClose: onDone) {
            PanelTitle(icon: .meds, tint: Palette.meds, label: Tracker.supplement.title)
        } content: {
            MedChipsRow(model: model, idPrefix: "meds", onAdd: { onSwitchTo(.supplement) }, onDone: onDone)
        }
        .tint(Palette.meds)
    }
}
