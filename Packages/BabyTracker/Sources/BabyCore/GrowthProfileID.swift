import CryptoKit
import Foundation

public enum GrowthProfileID {
    /// UUIDv5 with the child's UUID as namespace and the UTF-8 name
    /// `growth-profile-v1`. This contract must match every client and server.
    public static func forBaby(_ babyID: UUID) -> UUID {
        var namespace = babyID.uuid
        var input = withUnsafeBytes(of: &namespace) { Data($0) }
        input.append(contentsOf: "growth-profile-v1".utf8)
        // SHA-1 is required by UUIDv5; this ID is not a security credential.
        var bytes = Array(Insecure.SHA1.hash(data: input).prefix(16))
        bytes[6] = (bytes[6] & 0x0f) | 0x50
        bytes[8] = (bytes[8] & 0x3f) | 0x80
        return UUID(uuid: (
            bytes[0], bytes[1], bytes[2], bytes[3],
            bytes[4], bytes[5], bytes[6], bytes[7],
            bytes[8], bytes[9], bytes[10], bytes[11],
            bytes[12], bytes[13], bytes[14], bytes[15]
        ))
    }
}
