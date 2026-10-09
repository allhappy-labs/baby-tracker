import Darwin
import Foundation

public enum AppGroupBootstrapLock {
    public enum LockError: Error, Sendable, Equatable { case timedOut, cancelled, openFailed(Int32), lockFailed(Int32) }

    public static func withLock<T>(at url: URL, timeout: TimeInterval = 10, _ body: () throws -> T) throws -> T {
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        let descriptor = Darwin.open(url.path, O_CREAT | O_RDWR, S_IRUSR | S_IWUSR)
        guard descriptor >= 0 else { throw LockError.openFailed(errno) }
        defer { Darwin.close(descriptor) }
        let deadline = Date().addingTimeInterval(timeout)
        while flock(descriptor, LOCK_EX | LOCK_NB) != 0 {
            guard errno == EWOULDBLOCK else { throw LockError.lockFailed(errno) }
            guard !Task.isCancelled else { throw LockError.cancelled }
            guard Date() < deadline else { throw LockError.timedOut }
            usleep(10_000)
        }
        defer { flock(descriptor, LOCK_UN) }
        return try body()
    }
}
