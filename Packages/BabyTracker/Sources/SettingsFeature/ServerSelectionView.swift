import BabySync
import BabyUI
import SwiftUI

/// Chooses which server this device talks to.
///
/// Confirmation-first, in both directions. An origin is validated before it is
/// probed, and the probe's instance ID is shown for explicit confirmation
/// before any credential or record is sent — including when a *known* origin
/// starts reporting a different instance, which is the case that would
/// otherwise hand one server's data to another.
public struct ServerSelectionView: View {
    public typealias Probe = @Sendable (ServerOrigin) async throws -> ServerInfo

    @State private var input = ""
    @State private var pending: (origin: ServerOrigin, info: ServerInfo)?
    @State private var problem: String?
    @State private var isProbing = false

    private let hostedEndpoint: ConfirmedEndpoint?
    private let knownInstanceID: UUID?
    private let probe: Probe
    private let onConfirmed: (ConfirmedEndpoint) -> Void

    /// - Parameter prefilledOrigin: the server an invitation link named,
    ///   filled into the address field. Only filled in: the user still
    ///   checks it, and every check below still runs.
    public init(
        hostedEndpoint: ConfirmedEndpoint?,
        knownInstanceID: UUID?,
        prefilledOrigin: ServerOrigin? = nil,
        probe: @escaping Probe,
        onConfirmed: @escaping (ConfirmedEndpoint) -> Void
    ) {
        _input = State(initialValue: prefilledOrigin?.canonicalString ?? "")
        self.hostedEndpoint = hostedEndpoint
        self.knownInstanceID = knownInstanceID
        self.probe = probe
        self.onConfirmed = onConfirmed
    }

    public var body: some View {
        Form {
            if let hostedEndpoint {
                Section {
                    Button(Copy.useHosted) { checkHosted(hostedEndpoint) }
                        .frame(minHeight: 44)
                        .accessibilityIdentifier("server.useHosted")
                } header: {
                    Text(Copy.hostedHeader)
                }
            }

            Section {
                TextField(Copy.field, text: $input)
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    .keyboardType(.URL)
                    #endif
                    .autocorrectionDisabled()
                    .frame(minHeight: 44)
                    .accessibilityIdentifier("server.field")

                Button(Copy.check) { check() }
                    .frame(minHeight: 44)
                    .disabled(input.isEmpty || isProbing)
                    .accessibilityIdentifier("server.check")
            } header: {
                Text(Copy.customHeader)
            } footer: {
                Text(Copy.customFooter)
            }

            if let problem {
                Section {
                    Text(problem)
                        .foregroundStyle(Palette.destructive)
                        .accessibilityIdentifier("server.problem")
                }
            }

            if let pending {
                Section {
                    LabeledContent(Copy.originLabel, value: pending.origin.canonicalString)
                    LabeledContent(Copy.instanceLabel, value: pending.info.serverInstanceID.uuidString)
                    LabeledContent(Copy.versionLabel, value: pending.info.sourceVersion)

                    Button(Copy.confirm) {
                        onConfirmed(ConfirmedEndpoint(origin: pending.origin, serverInstanceID: pending.info.serverInstanceID))
                    }
                    .frame(minHeight: 44)
                    .accessibilityIdentifier("server.confirm")
                } header: {
                    Text(Copy.confirmHeader)
                } footer: {
                    Text(knownInstanceID == nil ? Copy.confirmFooter : Copy.instanceChangedFooter)
                }
            }
        }
        .navigationTitle(Copy.title)
    }

    private func check() {
        do {
            let origin = try ServerOrigin(validating: input)
            probeOrigin(origin, knownInstanceID: knownInstanceID, allowInstanceChange: true)
        } catch let error as ServerOrigin.ValidationError {
            problem = Self.describe(error)
        } catch {
            problem = Copy.unreachable
        }
    }

    private func checkHosted(_ endpoint: ConfirmedEndpoint) {
        probeOrigin(endpoint.origin, knownInstanceID: endpoint.serverInstanceID, allowInstanceChange: false)
    }

    private func probeOrigin(_ origin: ServerOrigin, knownInstanceID: UUID?, allowInstanceChange: Bool) {
        problem = nil
        pending = nil
        isProbing = true
        Task {
            defer { isProbing = false }
            do {
                let info = try await probe(origin)
                // Validate before offering: a mismatch or a redirect must be
                // refused here, not explained away in the confirmation sheet.
                let confirmed = try EndpointIdentityProbe().confirm(
                    origin: origin, info: info, knownInstanceID: knownInstanceID
                )
                pending = (confirmed.origin, info)
            } catch let error as EndpointIdentityError {
                switch error {
                case .instanceChanged(_, let reported):
                    problem = Copy.instanceChangedWarning
                    if allowInstanceChange {
                        pending = (origin, ServerInfoPlaceholder.make(origin: origin, instanceID: reported))
                    }
                case .originMismatch: problem = Copy.originMismatch
                case .redirected: problem = Copy.redirected
                }
            } catch {
                problem = Copy.unreachable
            }
        }
    }

    nonisolated static func describe(_ error: ServerOrigin.ValidationError) -> String {
        switch error {
        case .schemeNotHTTPS: String(localized: "Server addresses must start with https://.", bundle: .module)
        case .credentialsPresent: String(localized: "Remove the username and password from the address.", bundle: .module)
        case .pathPresent: String(localized: "Enter only the server address, with no path after it.", bundle: .module)
        case .queryPresent, .fragmentPresent: String(localized: "Enter only the server address, with nothing after it.", bundle: .module)
        case .missingHost, .notAURL: String(localized: "That doesn't look like a server address.", bundle: .module)
        case .invalidPort: String(localized: "That port number isn't valid.", bundle: .module)
        }
    }

    enum Copy {
        static let title = String(localized: "Server", bundle: .module)
        static let hostedHeader = String(localized: "Baby Tracker sync", bundle: .module)
        static let useHosted = String(localized: "Check Baby Tracker's server", bundle: .module)
        static let customHeader = String(localized: "Self-hosted server", bundle: .module)
        static let field = "https://sync.example.com"
        static let customFooter = String(localized: "Only https addresses are accepted, and the address must have no path, query, or sign-in details.", bundle: .module)
        static let check = String(localized: "Check server", bundle: .module)
        static let confirmHeader = String(localized: "Confirm this server", bundle: .module)
        static let confirmFooter = String(localized: "Check this matches the server you expect before signing in. Nothing has been sent yet.", bundle: .module)
        static let instanceChangedFooter = String(localized: "This address previously identified itself differently. Only continue if you know why it changed.", bundle: .module)
        static let instanceChangedWarning = String(localized: "This server's identity has changed since you last used it.", bundle: .module)
        static let originMismatch = String(localized: "That server identifies itself under a different address. It was not used.", bundle: .module)
        static let redirected = String(localized: "That address redirected somewhere else. It was not used.", bundle: .module)
        static let unreachable = String(localized: "That server could not be reached.", bundle: .module)
        static let originLabel = String(localized: "Address", bundle: .module)
        static let instanceLabel = String(localized: "Server ID", bundle: .module)
        static let versionLabel = String(localized: "Version", bundle: .module)
        static let confirm = String(localized: "Use this server", bundle: .module)
    }
}

/// Fills the confirmation rows when the probe was refused for a changed
/// instance: the user still needs to see which ID it now reports.
enum ServerInfoPlaceholder {
    static func make(origin: ServerOrigin, instanceID: UUID) -> ServerInfo {
        ServerInfo(
            serverInstanceID: instanceID,
            canonicalOrigin: origin.canonicalString,
            recoveryEpoch: 0,
            capabilities: [],
            appleAuthorizationMode: "unknown",
            maxItemsPerPage: 0,
            maxEncodedBytes: 0,
            sourceVersion: "unknown",
            sourceURL: ""
        )
    }
}
