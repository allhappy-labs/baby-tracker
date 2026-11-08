import SwiftUI
import BabyStore
import BabyUI

struct ImmunizationsScreen: View {
    let model: ImmunizationsViewModel
    let onEdit: (EntryRef) -> Void

    var body: some View {
        let presentation = model.presentation
        Group {
            if presentation.isEmpty {
                EmptyState(icon: .vaccine, title: ImmunizationsCopy.empty, message: nil)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .accessibilityIdentifier("immunizations.empty")
            } else {
                List {
                    ForEach(presentation.sections) { section in
                        Section {
                            ForEach(section.rows) { row in
                                Button { onEdit(.immunization(row.id)) } label: {
                                    Text(row.text)
                                        .textRole(.body)
                                        .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                                        .contentShape(Rectangle())
                                }
                                .buttonStyle(.plain)
                                .listRowBackground(Color.clear)
                                .accessibilityIdentifier("immunizations.row.\(row.id.uuidString)")
                            }
                        } header: {
                            Text(section.vaccine)
                                .textRole(.sectionLabel)
                                .foregroundStyle(Palette.vaccine)
                        }
                        .listSectionSeparator(.hidden, edges: .top)
                    }
                }
                .listStyle(.plain)
                .scrollContentBackground(.hidden)
                .accessibilityIdentifier("immunizations.screen")
            }
        }
        .background(Palette.ground.ignoresSafeArea())
        .foregroundStyle(Palette.primaryText)
        .navigationTitle(ImmunizationsCopy.title)
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar(.visible, for: .navigationBar)
        #endif
    }
}
