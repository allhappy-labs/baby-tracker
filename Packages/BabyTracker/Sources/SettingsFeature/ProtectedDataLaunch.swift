import BabyStore
import Foundation

/// Builds the app's one environment exactly once, and never before the
/// device's first unlock since it started.
///
/// A background launch (a push relaunching a terminated app) can come before
/// the first unlock after a restart. The App Group store and both
/// `UserDefaults` domains are unreadable then. A read answers nil, and code
/// that fills in a missing value (an installation ID, the volume unit) would
/// write or keep a fresh one over the parent's. Whatever is built then would
/// also be kept for the life of the process, which can outlast the unlock.
///
/// Once the device has been unlocked, it does not matter whether it is locked
/// again. Everything the environment reads is protected "until first user
/// authentication": the store and the defaults use the system's default file
/// protection, and the only keychain item is
/// `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`. A push to a phone that
/// is locked in a pocket — the usual case — must build and pull.
///
/// `UIApplication.isProtectedDataAvailable` cannot tell those two states
/// apart: it follows the stricter "complete" class and is false whenever a
/// passcode-protected device is locked. So it is only the fast path. When it
/// says no, the gate reads `FirstUnlockSentinel`, a file in that same
/// "until first user authentication" class: its contents can be read exactly
/// when the store and the defaults can.
///
/// The protected-data-available callback and the first scene connecting (a
/// scene needs an unlocked device) build it whatever the probe says. Until one
/// of these opens the gate, a launch or a push builds nothing and reads
/// nothing but the sentinel.
@MainActor
public final class ProtectedDataLaunch<Value> {
    private let isProtectedDataAvailable: @MainActor () -> Bool
    private let sentinel: FirstUnlockSentinel
    /// The sentinel was written by this process, while the device was unlocked.
    private var marked = false
    private var make: (@MainActor () -> Value)?
    /// The value, once built.
    public private(set) var value: Value?

    /// - Parameters:
    ///   - isProtectedDataAvailable: whether the device is unlocked now.
    ///   - sentinel: answers whether it has been unlocked since it started.
    public init(
        isProtectedDataAvailable: @escaping @MainActor () -> Bool,
        sentinel: FirstUnlockSentinel,
        make: @escaping @MainActor () -> Value
    ) {
        self.isProtectedDataAvailable = isProtectedDataAvailable
        self.sentinel = sentinel
        self.make = make
    }

    /// At launch, and when a push arrives: the value, built now if the device
    /// has been unlocked since it started, locked again or not. Nil before
    /// that, having built nothing and read nothing but the sentinel.
    public func valueIfUnlockedSinceBoot() -> Value? {
        if let value { return value }
        if isProtectedDataAvailable() {
            markUnlocked()
            return build()
        }
        guard sentinel.isReadable() else { return nil }
        return build()
    }

    /// Protected data just became available, or a scene connected: the value,
    /// built now if it was not yet.
    public func valueNow() -> Value {
        markUnlocked()
        if let value { return value }
        return build()
    }

    /// The device is unlocked now, so a later locked launch can tell that it
    /// has been unlocked since it started. Once per process; a failed write is
    /// tried again at the next unlock.
    private func markUnlocked() {
        guard !marked else { return }
        marked = sentinel.mark()
    }

    private func build() -> Value {
        guard let make else { preconditionFailure("built twice") }
        let built = make()
        value = built
        self.make = nil
        return built
    }
}

/// A small file whose contents can be read only after the device's first
/// unlock since it started.
///
/// It is written with `completeFileProtectionUntilFirstUserAuthentication`
/// (Data Protection class C). Apple: such a file "cannot be accessed until
/// after the device has booted. After the user unlocks the device for the
/// first time, your app can access the file and continue to access it even if
/// the user subsequently locks the device." That is the class the App Group
/// store and the defaults have (the system default; the app asks for no other),
/// and the class `kSecAttrAccessibleAfterFirstUnlock` keychain items share. So
/// a successful read means the environment's own reads will succeed too.
///
/// Only the contents are protected. The file's existence and attributes are
/// visible before the first unlock, so the probe reads the bytes and compares
/// them; it never asks whether the file exists.
///
/// The app writes it whenever it sees the device unlocked. Until it has once
/// (a fresh install, or the first launch of a build that introduced it), the
/// probe answers false and only an unlocked launch, the protected-data
/// callback or a scene opens the gate. Registering for pushes needs a built
/// environment, and every unlocked build writes the file.
public struct FirstUnlockSentinel: Sendable {
    /// The protection class the file is written with. "Complete" would make
    /// the probe fail whenever the device is locked, which is the defect this
    /// file exists to avoid.
    public static let protection: Data.WritingOptions = .completeFileProtectionUntilFirstUserAuthentication
    static let contents = Data("unlocked since boot\n".utf8)

    public let url: URL

    public init(url: URL) {
        self.url = url
    }

    /// The app's, beside the store in the App Group container.
    public static var app: FirstUnlockSentinel {
        FirstUnlockSentinel(url: BabySchema.defaultGroupDirectory.appending(path: "BabyTracker.first-unlock"))
    }

    /// Writes the file. Meant for when the device is unlocked; before the
    /// first unlock the class key is unavailable and the write fails, which
    /// leaves any earlier file as it was.
    /// - Returns: whether the file was written.
    @discardableResult
    public func mark() -> Bool {
        do {
            try Self.contents.write(to: url, options: [.atomic, Self.protection])
            return true
        } catch {
            return false
        }
    }

    /// Whether the device has been unlocked since it started, as far as the
    /// file can tell: its contents read back as written.
    public func isReadable() -> Bool {
        (try? Data(contentsOf: url)) == Self.contents
    }
}
