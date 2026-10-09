import CryptoKit
import Foundation

/// The P1 D6 nonce.
///
/// Byte-exact and deliberately literal: the domain separator, then a NUL, then
/// the canonical origin, a NUL, the lower-cased instance UUID, a NUL, and the
/// challenge's own unpadded-base64url *text*. The challenge is never decoded
/// before hashing, and the `\0` in the written spec is one byte, not the two
/// characters a backslash and a zero would give.
///
/// Binding origin and instance into the nonce is what stops a relay replaying
/// an Apple assertion obtained for one server against another.
public func appleNonce(canonicalOrigin: String, serverInstanceID: UUID, challenge: String) -> String {
    var bytes = Data("baby-tracker-auth-v1".utf8)
    bytes.append(0)
    bytes.append(contentsOf: canonicalOrigin.utf8)
    bytes.append(0)
    bytes.append(contentsOf: serverInstanceID.uuidString.lowercased().utf8)
    bytes.append(0)
    bytes.append(contentsOf: challenge.utf8)
    return SHA256.hash(data: bytes).map { String(format: "%02x", $0) }.joined()
}
