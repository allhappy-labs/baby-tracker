#if os(iOS)
import SwiftUI
import UIKit

/// The system share sheet for one file: Print, Save to Files, AirDrop,
/// Mail and the rest.
public struct ActivityShareSheet: UIViewControllerRepresentable {
    let file: URL

    public init(file: URL) { self.file = file }

    public func makeUIViewController(context: Context) -> UIActivityViewController {
        UIActivityViewController(activityItems: [file], applicationActivities: nil)
    }

    public func updateUIViewController(_ controller: UIActivityViewController, context: Context) {}
}
#endif
