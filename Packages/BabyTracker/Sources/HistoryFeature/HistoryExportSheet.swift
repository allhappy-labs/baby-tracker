import SwiftUI
import BabyUI

/// Export PDF: a range — a preset or two dates — then Create PDF, which
/// opens the share sheet with the file.
///
/// Accessibility identifiers: `history.export.preset`, `history.export.from`,
/// `history.export.to`, `history.export.create`, `history.export.cancel`,
/// `history.export.empty`, `history.export.failed`.
struct HistoryExportSheet: View {
    @State var model: HistoryExportViewModel
    let onDone: () -> Void

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Picker(ExportCopy.range, selection: Binding(get: { model.preset }, set: { model.select($0) })) {
                        ForEach(HistoryExportPreset.allCases) { Text(ExportCopy.name($0)).tag($0) }
                    }
                    .pickerStyle(.segmented)
                    .accessibilityIdentifier("history.export.preset")
                    if model.preset == .custom {
                        DatePicker(ExportCopy.from, selection: Binding(get: { model.from }, set: { model.setFrom($0) }),
                                   in: model.earliest...model.latest, displayedComponents: .date)
                            .accessibilityIdentifier("history.export.from")
                        DatePicker(ExportCopy.to, selection: Binding(get: { model.to }, set: { model.setTo($0) }),
                                   in: model.earliest...model.latest, displayedComponents: .date)
                            .accessibilityIdentifier("history.export.to")
                    }
                }
                Section {
                    Button { Task { await model.create() } } label: {
                        HStack {
                            Text(ExportCopy.create)
                            Spacer()
                            if model.state == .working { ProgressView() }
                        }
                    }
                    .disabled(model.state == .working)
                    .accessibilityIdentifier("history.export.create")
                    switch model.state {
                    case .nothingToExport:
                        Text(ExportCopy.nothing)
                            .foregroundStyle(Palette.secondaryText)
                            .accessibilityIdentifier("history.export.empty")
                    case .failed:
                        Text(ExportCopy.failed)
                            .foregroundStyle(Palette.destructive)
                            .accessibilityIdentifier("history.export.failed")
                    default:
                        EmptyView()
                    }
                }
            }
            .navigationTitle(HistoryCopy.exportPDF)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(ExportCopy.cancel, action: onDone)
                        .accessibilityIdentifier("history.export.cancel")
                }
            }
        }
        .presentationDetents([.medium, .large])
        #if os(iOS)
        .sheet(item: shareItem, onDismiss: { model.shareDidFinish() }) { item in
            ActivityShareSheet(file: item.url)
        }
        #endif
    }

    #if os(iOS)
    private var shareItem: Binding<SharedPDF?> {
        Binding(
            get: { if case .ready(let url) = model.state { SharedPDF(url: url) } else { nil } },
            set: { if $0 == nil { model.shareDidFinish() } }
        )
    }
    #endif
}

private struct SharedPDF: Identifiable {
    let url: URL
    var id: URL { url }
}

private enum ExportCopy {
    static let range = L10n.tr("Range")
    static let from = L10n.tr("From")
    static let to = L10n.tr("To")
    static let create = L10n.tr("Create PDF")
    static let cancel = L10n.tr("Cancel")
    static let nothing = L10n.tr("Nothing logged in these dates")
    static let failed = L10n.tr("Couldn't create the PDF. Try again.")

    static func name(_ preset: HistoryExportPreset) -> String {
        switch preset {
        case .week: L10n.tr("7 days")
        case .fortnight: L10n.tr("14 days")
        case .month: L10n.tr("30 days")
        case .custom: L10n.tr("Custom")
        }
    }
}
