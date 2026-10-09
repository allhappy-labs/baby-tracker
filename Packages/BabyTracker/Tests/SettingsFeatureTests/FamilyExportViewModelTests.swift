import BabyProtocol
import BabySync
import Foundation
import Testing
@testable import SettingsFeature

private final class ScriptedExportClient: ExportServing, @unchecked Sendable {
    let job: ExportStatusDTO
    let ready: ExportStatusDTO
    let file: URL

    init(job: ExportStatusDTO, ready: ExportStatusDTO, file: URL) {
        self.job = job; self.ready = ready; self.file = file
    }

    func request(requestID: UUID) async throws -> ExportStatusDTO { job }
    func status(id: UUID) async throws -> ExportStatusDTO { ready }
    func download(id: UUID) async throws -> URL { file }
    func removeTemporaryFile(_ url: URL) throws { try FileManager.default.removeItem(at: url) }
    func removeTemporaryFiles() throws {}
}

@MainActor
@Test func exportModelWaitsForReadyAndRemovesSharedFile() async throws {
    let id = UUID(uuidString: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")!
    func status(_ state: String) throws -> ExportStatusDTO {
        let json = """
        {"protocol_version":"v1","export_id":"\(id.uuidString.lowercased())","state":"\(state)","created_at":"2026-09-27T10:00:00.000000Z","expires_at":"2026-09-28T10:00:00.000000Z","plaintext_bytes":2,"sha256":"00"}
        """
        return try JSONDecoder().decode(ExportStatusDTO.self, from: Data(json.utf8))
    }
    let file = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try Data("{}".utf8).write(to: file)
    defer { try? FileManager.default.removeItem(at: file) }
    let suite = "family-export-test.\(UUID().uuidString)"
    let defaults = UserDefaults(suiteName: suite)!
    defer { defaults.removePersistentDomain(forName: suite) }
    let client = ScriptedExportClient(job: try status("queued"), ready: try status("ready"), file: file)
    let model = FamilyExportViewModel(client: client, persistenceKey: "family", defaults: defaults)
    await model.request()
    #expect(model.state == .preparing)
    let resumed = FamilyExportViewModel(client: client, persistenceKey: "family", defaults: defaults)
    #expect(resumed.exportID == id)
    #expect(resumed.state == .preparing)
    await resumed.refresh()
    #expect(resumed.state == .ready)
    await resumed.downloadAndShare()
    #expect(resumed.state == .sharing)
    resumed.shareDidFinish()
    #expect(resumed.state == .ready)
    #expect(!FileManager.default.fileExists(atPath: file.path))
}
