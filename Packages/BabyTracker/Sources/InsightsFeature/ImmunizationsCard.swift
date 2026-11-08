import SwiftUI
import BabyStore
import BabyUI

enum ImmunizationsCopy {
    static let title = L10n.tr("Immunizations")
    static let empty = L10n.tr("No vaccinations logged yet")
    static let open = L10n.tr("Open immunizations")
}

struct ImmunizationsCard: View {
    let model: ImmunizationsViewModel
    let onEdit: (EntryRef) -> Void
    /// Held here, not by a link inside the row: the row goes when the last
    /// shot is deleted, and the screen must stay to say there are none.
    @State private var isShowingScreen = false

    var body: some View {
        let presentation = model.presentation
        Card {
            VStack(alignment: .leading, spacing: Spacing.s) {
                // The icon alone heads the card, as it does on every other card here.
                TrackerIcon(.vaccine).foregroundStyle(Palette.vaccine).accessibilityHidden(true)
                if let latest = presentation.latestText {
                    Text(latest).textRole(.caption)
                        .accessibilityIdentifier("insights.immunizations.latest")
                    Button { isShowingScreen = true } label: {
                        ListRow(ImmunizationsCopy.open, value: "\(presentation.count)",
                                accessory: .disclosure, density: .compact)
                            .cardRow()
                    }
                    .buttonStyle(.plain)
                    .accessibilityIdentifier("insights.immunizations.open")
                } else {
                    Text(ImmunizationsCopy.empty).textRole(.caption)
                        .accessibilityIdentifier("insights.immunizations.empty")
                }
            }
        }
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("insights.immunizations")
        .navigationDestination(isPresented: $isShowingScreen) {
            ImmunizationsScreen(model: model, onEdit: onEdit)
        }
    }
}
