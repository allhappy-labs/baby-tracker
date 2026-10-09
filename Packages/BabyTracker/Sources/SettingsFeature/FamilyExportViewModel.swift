import BabyProtocol
import BabySync
import Foundation
import Observation

@MainActor
@Observable
public final class FamilyExportViewModel {
    private struct SavedJob: Codable {
        let requestID: UUID?
        let exportID: UUID?
        let expiresAt: String?
    }

    public enum State: Equatable {
        case idle, preparing, ready, sharing, expired, failed(String)
    }

    public private(set) var state: State = .idle
    public private(set) var exportID: UUID?
    public private(set) var expiresAt: String?
    public private(set) var sharedFile: URL?
    private let client: any ExportServing
    @ObservationIgnored private var pendingRequestID: UUID?
    @ObservationIgnored private let persistenceKey: String?
    @ObservationIgnored private let defaults: UserDefaults

    public init(client: any ExportServing, persistenceKey: String? = nil, defaults: UserDefaults = .standard) {
        self.client = client
        self.persistenceKey = persistenceKey
        self.defaults = defaults
        if let persistenceKey, let raw = defaults.data(forKey: persistenceKey),
           let saved = try? JSONDecoder().decode(SavedJob.self, from: raw) {
            pendingRequestID = saved.requestID
            exportID = saved.exportID
            expiresAt = saved.expiresAt
            if saved.exportID != nil { state = .preparing }
        }
    }

    public func request() async {
        guard state != .preparing && state != .sharing else { return }
        if state == .expired || exportID != nil {
            pendingRequestID = nil
            exportID = nil
            expiresAt = nil
        }
        let requestID = pendingRequestID ?? UUID()
        pendingRequestID = requestID
        persist()
        state = .preparing
        do {
            let status = try await client.request(requestID: requestID)
            apply(status)
        } catch {
            state = .failed(Self.message(for: error))
        }
    }

    public func refresh() async {
        guard let exportID else { return }
        do {
            apply(try await client.status(id: exportID))
        } catch {
            state = .failed(Self.message(for: error))
        }
    }

    public func downloadAndShare() async {
        guard state == .ready, let exportID else { return }
        state = .sharing
        do {
            sharedFile = try await client.download(id: exportID)
            state = .sharing
        } catch {
            state = .failed(Self.message(for: error))
        }
    }

    public func shareDidFinish() {
        if let sharedFile { try? client.removeTemporaryFile(sharedFile) }
        sharedFile = nil
        if state == .sharing { state = .ready }
    }

    public func leaveScreen() {
        shareDidFinish()
        try? client.removeTemporaryFiles()
    }

    private func apply(_ status: ExportStatusDTO) {
        exportID = status.exportID
        expiresAt = status.expiresAt
        persist()
        switch status.state {
        case .queued, .running: state = .preparing
        case .ready: state = .ready
        case .expired: state = .expired
        case .failed: state = .failed(Self.message(forCode: status.code))
        }
    }

    private func persist() {
        guard let persistenceKey else { return }
        let saved = SavedJob(requestID: pendingRequestID, exportID: exportID, expiresAt: expiresAt)
        if let raw = try? JSONEncoder().encode(saved) {
            defaults.set(raw, forKey: persistenceKey)
        }
    }

    private static func message(for error: Error) -> String {
        guard let error = error as? ExportClientError else { return String(localized: "The export could not be completed. Try again.", bundle: .module) }
        switch error {
        case .rateLimited(let retryAfter):
            let hours = max(1, Int(ceil(retryAfter / 3600)))
            let key = hours == 1 ? "Another export can be requested in about %d hour." : "Another export can be requested in about %d hours."
            return String(format: NSLocalizedString(key, bundle: .module, comment: ""), hours)
        case .ownerRequired, .requesterMismatch, .authorizationChanged:
            return String(localized: "You no longer have access to this export.", bundle: .module)
        case .expired: return String(localized: "This export expired. Request a new one.", bundle: .module)
        case .tooLarge: return String(localized: "This family's data is too large to export.", bundle: .module)
        case .notReady: return String(localized: "The export is still being prepared.", bundle: .module)
        case .integrity: return String(localized: "The downloaded file did not pass verification. Try again.", bundle: .module)
        case .server(let code): return message(forCode: code)
        }
    }

    private static func message(forCode code: String?) -> String {
        switch code {
        case "export_too_large": String(localized: "This family's data is too large to export.", bundle: .module)
        case "export_authorization_changed": String(localized: "You no longer have access to this export.", bundle: .module)
        default: String(localized: "The export could not be completed. Try again.", bundle: .module)
        }
    }
}
