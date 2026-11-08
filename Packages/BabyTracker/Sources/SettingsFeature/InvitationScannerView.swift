import BabyCore
import BabySync
import BabyUI
import SwiftUI
#if os(iOS)
import AVFoundation
import UIKit
import VisionKit
#endif

/// Full-screen QR scanner for an invitation, from the no-family section of
/// the Caregivers screen. Only presented when `CameraScanning.access()` says
/// `.scan`:
/// where the camera cannot be used, the section says so inline instead
/// (`ScannerUnavailableRows`), rather than opening a black screen.
///
/// The first QR code that is an invitation closes the scanner and goes on to
/// the join confirmation; anything else gets a hint and scanning continues.
/// A scanner that fails to start hands back to the inline message.
struct InvitationScannerView: View {
    let confirmedOrigin: ServerOrigin
    let onScanned: (InvitationIntent) -> Void
    let onUnavailable: () -> Void
    let onClose: () -> Void

    @State private var hint: String?
    @State private var hasScanned = false

    var body: some View {
        SheetScaffold(
            InvitationScannerCopy.title,
            closeTitle: InvitationScannerCopy.close,
            closeIdentifier: "scanner.close",
            onClose: onClose
        ) {
            content
        }
    }

    @ViewBuilder
    private var content: some View {
        #if os(iOS)
        ZStack(alignment: .bottom) {
            InvitationDataScanner(onPayloads: handle, onStartFailed: onUnavailable)
            if let hint {
                Text(hint)
                    .font(.callout)
                    .foregroundStyle(Palette.primaryText)
                    .padding(.horizontal, Spacing.l)
                    .padding(.vertical, Spacing.m)
                    .background(Palette.ground, in: Capsule())
                    .padding(Spacing.l)
                    .accessibilityIdentifier("scanner.hint")
            }
        }
        .aspectRatio(3 / 4, contentMode: .fit)
        .clipShape(RoundedRectangle(cornerRadius: Radius.card, style: .continuous))
        #else
        Color.clear.onAppear(perform: onUnavailable)
        #endif
    }

    private func handle(_ payloads: [String]) {
        guard !hasScanned else { return }
        switch InvitationScanning.outcome(for: payloads, confirmedOrigin: confirmedOrigin) {
        case .nothing:
            break
        case .notAnInvitation:
            hint = InvitationScannerCopy.notAnInvitation
        case .invitation(let intent):
            hasScanned = true
            onScanned(intent)
        }
    }
}

/// Said in the join section, under "Scan QR code", when the camera cannot be
/// used — the simulator, a device without one, access denied or restricted,
/// or a scanner that would not start. The paste field is right below it.
struct ScannerUnavailableRows: View {
    let cameraDenied: Bool
    @Environment(\.openURL) private var openURL

    var body: some View {
        Label(InvitationScannerCopy.cameraUnavailable, systemImage: "camera")
            .font(.callout)
            .foregroundStyle(Palette.secondaryText)
            .accessibilityElement(children: .combine)
            .accessibilityIdentifier("scanner.unavailable")
        #if os(iOS)
        if cameraDenied, let settings = URL(string: UIApplication.openSettingsURLString) {
            Button(InvitationScannerCopy.openSettings) { openURL(settings) }
                .frame(minHeight: 44)
                .accessibilityIdentifier("scanner.openSettings")
        }
        #endif
    }
}

/// The inline "camera isn't available" message, and whether it offers
/// Settings because the camera was refused.
struct ScannerUnavailableNotice: Equatable {
    let showSettings: Bool
}

/// Camera permission as the scanner decision needs it, so the decision can
/// be tested without AVFoundation.
enum CameraAuthorization: Equatable {
    case notDetermined, authorized, denied, restricted
}

/// What tapping "Scan QR code" should do.
enum ScannerAccess: Equatable {
    /// Nobody has been asked for the camera yet: ask, then decide again.
    case requestAccess
    case scan
    /// Say so inline; `showSettings` when the user can change it there.
    case unavailable(showSettings: Bool)

    /// `DataScannerViewController.isAvailable` is only true once camera
    /// access has been granted, so on a fresh install it is false until the
    /// user has been asked — asking comes before believing it.
    static func decide(authorization: CameraAuthorization, isSupported: Bool, isAvailable: Bool) -> ScannerAccess {
        guard isSupported else { return .unavailable(showSettings: false) }
        switch authorization {
        case .notDetermined: return .requestAccess
        case .denied, .restricted: return .unavailable(showSettings: true)
        case .authorized: return isAvailable ? .scan : .unavailable(showSettings: false)
        }
    }
}

/// Whether this device can scan, asking for the camera first if nobody has.
@MainActor
enum CameraScanning {
    /// Never `.requestAccess`: that is resolved here, by asking.
    static func access() async -> ScannerAccess {
        let first = current()
        guard first == .requestAccess else { return first }
        #if os(iOS)
        _ = await AVCaptureDevice.requestAccess(for: .video)
        #endif
        let second = current()
        return second == .requestAccess ? .unavailable(showSettings: false) : second
    }

    static var accessDenied: Bool {
        authorization == .denied || authorization == .restricted
    }

    private static func current() -> ScannerAccess {
        #if os(iOS)
        ScannerAccess.decide(
            authorization: authorization,
            isSupported: DataScannerViewController.isSupported,
            isAvailable: DataScannerViewController.isAvailable
        )
        #else
        ScannerAccess.decide(authorization: authorization, isSupported: false, isAvailable: false)
        #endif
    }

    private static var authorization: CameraAuthorization {
        #if os(iOS)
        switch AVCaptureDevice.authorizationStatus(for: .video) {
        case .authorized: .authorized
        case .denied: .denied
        case .restricted: .restricted
        default: .notDetermined
        }
        #else
        .notDetermined
        #endif
    }
}

extension View {
    /// Full screen where there is one; the package's macOS test build gets a
    /// sheet, since the scanner itself only exists on iOS.
    func scannerCover<Content: View>(
        isPresented: Binding<Bool>, onDismiss: @escaping () -> Void, @ViewBuilder content: @escaping () -> Content
    ) -> some View {
        #if os(iOS)
        fullScreenCover(isPresented: isPresented, onDismiss: onDismiss, content: content)
        #else
        sheet(isPresented: isPresented, onDismiss: onDismiss, content: content)
        #endif
    }
}

enum InvitationScannerCopy {
    static let title = String(localized: "Scan QR code", bundle: .module)
    static let close = String(localized: "Close", bundle: .module)
    static let notAnInvitation = String(localized: "That QR code isn't a Baby Tracker invitation", bundle: .module)
    static let cameraUnavailable = String(localized: "Camera isn't available. Paste the code instead.", bundle: .module)
    static let openSettings = String(localized: "Open Settings", bundle: .module)
}

#if os(iOS)
/// VisionKit's scanner, recognising QR codes only, started when it appears
/// and stopped when it goes.
private struct InvitationDataScanner: UIViewControllerRepresentable {
    let onPayloads: ([String]) -> Void
    let onStartFailed: () -> Void

    func makeCoordinator() -> Coordinator { Coordinator(onPayloads: onPayloads) }

    func makeUIViewController(context: Context) -> ScannerHost {
        let scanner = DataScannerViewController(
            recognizedDataTypes: [.barcode(symbologies: [.qr])],
            qualityLevel: .balanced,
            isHighFrameRateTrackingEnabled: false,
            isGuidanceEnabled: true
        )
        scanner.delegate = context.coordinator
        return ScannerHost(scanner: scanner, onStartFailed: onStartFailed)
    }

    func updateUIViewController(_ host: ScannerHost, context: Context) {
        context.coordinator.onPayloads = onPayloads
    }

    @MainActor
    final class Coordinator: NSObject, DataScannerViewControllerDelegate {
        var onPayloads: ([String]) -> Void

        init(onPayloads: @escaping ([String]) -> Void) { self.onPayloads = onPayloads }

        func dataScanner(
            _ dataScanner: DataScannerViewController, didAdd addedItems: [RecognizedItem], allItems: [RecognizedItem]
        ) {
            let payloads = addedItems.compactMap { item -> String? in
                guard case .barcode(let barcode) = item else { return nil }
                return barcode.payloadStringValue
            }
            onPayloads(payloads)
        }
    }

    /// Holds the scanner as a child so it can start on appear and stop on
    /// disappear — `DataScannerViewController` does neither by itself.
    final class ScannerHost: UIViewController {
        let scanner: DataScannerViewController
        let onStartFailed: () -> Void

        init(scanner: DataScannerViewController, onStartFailed: @escaping () -> Void) {
            self.scanner = scanner
            self.onStartFailed = onStartFailed
            super.init(nibName: nil, bundle: nil)
        }

        @available(*, unavailable)
        required init?(coder: NSCoder) { fatalError("init(coder:) is not supported") }

        override func viewDidLoad() {
            super.viewDidLoad()
            addChild(scanner)
            scanner.view.frame = view.bounds
            scanner.view.autoresizingMask = [.flexibleWidth, .flexibleHeight]
            view.addSubview(scanner.view)
            scanner.didMove(toParent: self)
        }

        override func viewDidAppear(_ animated: Bool) {
            super.viewDidAppear(animated)
            do {
                try scanner.startScanning()
            } catch {
                AppLog.ui.error("invitation scanner did not start: \(String(describing: error), privacy: .public)")
                onStartFailed()
            }
        }

        override func viewWillDisappear(_ animated: Bool) {
            super.viewWillDisappear(animated)
            scanner.stopScanning()
        }
    }
}
#endif
