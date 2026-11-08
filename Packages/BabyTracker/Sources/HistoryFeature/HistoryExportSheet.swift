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
        SheetScaffold(
            HistoryCopy.exportPDF,
            closeTitle: ExportCopy.cancel,
            closeIdentifier: "history.export.cancel",
            onClose: onDone,
            primary: SheetAction(ExportCopy.create, identifier: "history.export.create",
                                 isEnabled: model.state != .working) { Task { await model.create() } }
        ) {
            CardSection {
                Picker(ExportCopy.range, selection: Binding(get: { model.preset }, set: { model.select($0) })) {
                    ForEach(HistoryExportPreset.allCases) { Text(ExportCopy.name($0)).tag($0) }
                }
                .pickerStyle(.segmented)
                .accessibilityIdentifier("history.export.preset")
            }
            // Always in the tree, collapsed until Custom: a compact date picker
            // inserted into a fitted sheet already on screen keeps a stale
            // layout, its pill drawn off to the side.
            CardSection {
                dateRow(ExportCopy.from, identifier: "history.export.from",
                        selection: Binding(get: { model.from }, set: { model.setFrom($0) }))
                dateRow(ExportCopy.to, identifier: "history.export.to",
                        selection: Binding(get: { model.to }, set: { model.setTo($0) }))
            }
            .frame(maxHeight: model.preset == .custom ? nil : 0, alignment: .top)
            .clipped()
            .opacity(model.preset == .custom ? 1 : 0)
            .accessibilityHidden(model.preset != .custom)
            switch model.state {
            case .working:
                ProgressView().frame(maxWidth: .infinity)
            case .nothingToExport:
                Text(ExportCopy.nothing)
                    .textRole(.caption)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .accessibilityIdentifier("history.export.empty")
            case .failed:
                InlineError(ExportCopy.failed)
                    .accessibilityIdentifier("history.export.failed")
            default:
                EmptyView()
            }
        }
        #if os(iOS)
        .sheet(item: shareItem, onDismiss: { model.shareDidFinish() }) { item in
            ActivityShareSheet(file: item.url)
        }
        #endif
        // A fitted sheet does not inherit RootView's tint.
        .tint(Palette.feed)
    }

    /// A date row: the label beside the picker, over it at the accessibility
    /// sizes. The picker is offered the row's leftover width and pinned to the
    /// trailing edge; a fitted sheet otherwise measures it wider than the row.
    private func dateRow(_ title: String, identifier: String, selection: Binding<Date>) -> some View {
        LabeledRow(title) {
            DatePicker(title, selection: selection, in: model.earliest...model.latest, displayedComponents: .date)
                .datePickerStyle(.compact)
                .labelsHidden()
                .accessibilityLabel(title)
                .accessibilityIdentifier(identifier)
        }
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
