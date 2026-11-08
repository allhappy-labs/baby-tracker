import BabySync
import BabyUI
import SwiftUI
#if os(iOS)
import UIKit
#endif

/// Creates an owner export, waits for the server to prepare it, then shares the verified file.
public struct FamilyExportView: View {
    @State private var model: FamilyExportViewModel
    @State private var downloadTask: Task<Void, Never>?
    @Environment(\.scenePhase) private var scenePhase

    public init(client: any ExportServing, persistenceKey: String? = nil) {
        _model = State(initialValue: FamilyExportViewModel(client: client, persistenceKey: persistenceKey))
    }

    public var body: some View {
        SettingsPage {
            CardSection {
                Text(FamilyExportCopy.introduction)
                    .font(.callout)
                    .fixedSize(horizontal: false, vertical: true)
                switch model.state {
                case .idle:
                    ActionRow(FamilyExportCopy.request, identifier: "export.request") { Task { await model.request() } }
                case .preparing:
                    ProgressView(FamilyExportCopy.preparing)
                        .accessibilityIdentifier("export.preparing")
                case .ready:
                    ActionRow(FamilyExportCopy.downloadAndShare, identifier: "export.download") {
                        downloadTask = Task {
                            await model.downloadAndShare()
                            downloadTask = nil
                        }
                    }
                case .sharing:
                    ProgressView(FamilyExportCopy.sharing)
                case .expired:
                    Text(FamilyExportCopy.expired)
                    ActionRow(FamilyExportCopy.requestNew, identifier: "export.requestNew") { Task { await model.request() } }
                case .failed(let message):
                    InlineError(message)
                    ActionRow(FamilyExportCopy.tryAgain, identifier: "export.tryAgain") { Task { await model.request() } }
                }
            } footer: {
                Text(FamilyExportCopy.sharingFooter)
            }
        }
        .navigationTitle(FamilyExportCopy.title)
        .task(id: model.exportID) {
            if model.exportID != nil, model.state == .preparing { await model.refresh() }
            while !Task.isCancelled, model.state == .preparing, model.exportID != nil {
                try? await Task.sleep(for: .seconds(2))
                if Task.isCancelled { break }
                await model.refresh()
            }
        }
        #if os(iOS)
        .sheet(item: sharePresentation, onDismiss: { model.shareDidFinish() }) { share in
            ActivityShareSheet(file: share.url)
        }
        #else
        .safeAreaInset(edge: .bottom) {
            if let file = model.sharedFile {
                ShareLink(item: file) { Text(FamilyExportCopy.share) }
            }
        }
        #endif
        .onChange(of: scenePhase) { _, phase in
            if phase != .active {
                downloadTask?.cancel()
                model.leaveScreen()
            }
        }
        .onDisappear {
            downloadTask?.cancel()
            model.leaveScreen()
        }
    }

    #if os(iOS)
    private var sharePresentation: Binding<ExportShareItem?> {
        Binding(
            get: { model.sharedFile.map(ExportShareItem.init) },
            set: { if $0 == nil { model.shareDidFinish() } }
        )
    }
    #endif
}

private enum FamilyExportCopy {
    static let introduction = String(localized: "Download a JSON copy of your family's records. Only the owner can request or download it. The file is available for 24 hours after you request it.", bundle: .module)
    static let request = String(localized: "Request export", bundle: .module)
    static let preparing = String(localized: "Preparing export…", bundle: .module)
    static let downloadAndShare = String(localized: "Download and share", bundle: .module)
    static let sharing = String(localized: "Sharing export…", bundle: .module)
    static let expired = String(localized: "This export expired.", bundle: .module)
    static let requestNew = String(localized: "Request a new export", bundle: .module)
    static let tryAgain = String(localized: "Try again", bundle: .module)
    static let sharingFooter = String(localized: "The shared file contains sensitive family information. Choose a trusted destination.", bundle: .module)
    static let title = String(localized: "Export family data", bundle: .module)
    static let share = String(localized: "Share export", bundle: .module)
}

#if os(iOS)
private struct ExportShareItem: Identifiable {
    let url: URL
    var id: URL { url }
    init(_ url: URL) { self.url = url }
}
#endif
