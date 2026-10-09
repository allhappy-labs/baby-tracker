import Foundation

/// Every read of "now" in the app goes through this, so timer behaviour is
/// testable without waiting. Named DateProvider rather than Clock to avoid
/// colliding with the standard library's Clock protocol.
public protocol DateProvider: Sendable {
    var now: Date { get }
}

public struct SystemDateProvider: DateProvider {
    public init() {}
    public var now: Date { Date() }
}

/// Test double. Time moves only when the test says so.
public final class TestDateProvider: DateProvider, @unchecked Sendable {
    private let lock = NSLock()
    private var current: Date

    public init(_ start: Date) {
        self.current = start
    }

    public var now: Date {
        lock.withLock { current }
    }

    public func advance(by interval: TimeInterval) {
        lock.withLock { current += interval }
    }
}
