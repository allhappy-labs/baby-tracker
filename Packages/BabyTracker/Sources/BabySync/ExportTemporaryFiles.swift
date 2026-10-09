import Foundation

/// A dedicated app-private area. Only files created for exports are removed.
public final class ExportTemporaryFiles: @unchecked Sendable {
    public let directory: URL
    private let manager = FileManager.default

    public init(directory: URL? = nil) throws {
        let base = directory ?? FileManager.default.temporaryDirectory
            .appendingPathComponent("baby-tracker-family-exports", isDirectory: true)
        self.directory = base
        try manager.createDirectory(at: base, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var mutable = base
        try mutable.setResourceValues(values)
    }

    public func removeAll() throws {
        for file in try contents() { try manager.removeItem(at: file) }
    }

    public func remove(_ file: URL) throws {
        guard file.deletingLastPathComponent().standardizedFileURL == directory.standardizedFileURL else { return }
        if manager.fileExists(atPath: file.path) { try manager.removeItem(at: file) }
    }

    public func contents() throws -> [URL] {
        try manager.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil)
    }

    func createFile() throws -> URL {
        let file = directory.appendingPathComponent(UUID().uuidString.lowercased() + ".json")
        guard manager.createFile(atPath: file.path, contents: nil, attributes: [.posixPermissions: 0o600]) else {
            throw CocoaError(.fileWriteUnknown)
        }
        #if os(iOS)
        try manager.setAttributes([.protectionKey: FileProtectionType.complete], ofItemAtPath: file.path)
        #endif
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var mutable = file
        try mutable.setResourceValues(values)
        return file
    }
}
