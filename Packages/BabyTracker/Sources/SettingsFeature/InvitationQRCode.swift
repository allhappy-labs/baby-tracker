import BabySync
import CoreImage
import CoreImage.CIFilterBuiltins
import Foundation
import SwiftUI

/// The QR code an owner holds up for the other phone's camera.
///
/// It encodes the app's own `babytracker://invite` link rather than the https
/// share link: the iPhone Camera app opens a custom-scheme link straight in
/// Baby Tracker, and the in-app scanner hands the same text to
/// `InvitationParser`. Rendered on the phone with CoreImage — no network, and
/// the token never leaves the device to be drawn.
public enum InvitationQRCode {
    /// Everything in a query value except the characters RFC 3986 leaves
    /// unreserved, so the origin's `:` and `/` cannot be read as part of the
    /// link's own structure.
    private static let queryValueAllowed = CharacterSet(
        charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
    )

    /// `babytracker://invite?server=<percent-encoded origin>&token=<token>`.
    ///
    /// Parsed from the invitation's own share link, so the origin is the one
    /// the server minted it for — never this phone's guess — and a link that
    /// is not an invitation throws instead of producing a code that would.
    public static func payload(for invitation: FamilyInvitation) throws -> String {
        let intent = try InvitationParser.parse(invitation.shareURL.absoluteString)
        let server = intent.origin.canonicalString.addingPercentEncoding(withAllowedCharacters: queryValueAllowed)
            ?? intent.origin.canonicalString
        // The token needs no encoding: the parser has already refused
        // anything but letters, digits, `-` and `_`.
        return "\(InvitationParser.customScheme)://invite?server=\(server)&token=\(intent.token)"
    }

    /// The payload as a crisp black-on-white code, ready to draw.
    public static func image(for payload: String) -> Image? {
        cgImage(for: payload).map { Image(decorative: $0, scale: 1) }
    }

    /// Correction level "M" — enough to survive a scratched screen or glare
    /// without making the code dense — scaled by whole modules with
    /// nearest-neighbour sampling, so no module edge is ever blurred.
    static func cgImage(for payload: String, pointsPerModule: CGFloat = 12) -> CGImage? {
        let filter = CIFilter.qrCodeGenerator()
        filter.message = Data(payload.utf8)
        filter.correctionLevel = "M"
        guard let output = filter.outputImage else { return nil }
        let scaled = output
            .samplingNearest()
            .transformed(by: CGAffineTransform(scaleX: pointsPerModule, y: pointsPerModule))
        return CIContext(options: [.useSoftwareRenderer: false]).createCGImage(scaled, from: scaled.extent)
    }
}
