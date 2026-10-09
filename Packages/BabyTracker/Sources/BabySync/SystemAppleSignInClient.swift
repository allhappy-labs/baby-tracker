#if canImport(AuthenticationServices)
import AuthenticationServices
#endif
import Foundation

#if canImport(AuthenticationServices) && canImport(UIKit)
import UIKit

/// Sign in with Apple, driven by `ASAuthorizationController`.
///
/// The nonce is passed through untouched: it is already the hex SHA-256 the P1
/// D6 vector specifies, and Apple hashes whatever string it is handed. Hashing
/// it a second time here would produce an assertion the server cannot verify.
@MainActor
public final class SystemAppleSignInClient: NSObject, AppleSignInClient {
    private var continuation: CheckedContinuation<AppleProof, any Error>?

    public override init() { super.init() }

    public nonisolated func requestProof(nonce: String) async throws -> AppleProof {
        try await MainActor.run { () -> Void in }
        return try await withCheckedThrowingContinuation { continuation in
            Task { @MainActor in
                self.continuation = continuation
                let request = ASAuthorizationAppleIDProvider().createRequest()
                request.requestedScopes = [.fullName]
                request.nonce = nonce
                let controller = ASAuthorizationController(authorizationRequests: [request])
                controller.delegate = self
                controller.presentationContextProvider = self
                controller.performRequests()
            }
        }
    }

    public enum SignInError: Error, Sendable, Equatable {
        case missingIdentityToken
        case unexpectedCredential
    }
}

extension SystemAppleSignInClient: ASAuthorizationControllerDelegate {
    public func authorizationController(
        controller: ASAuthorizationController,
        didCompleteWithAuthorization authorization: ASAuthorization
    ) {
        guard let credential = authorization.credential as? ASAuthorizationAppleIDCredential else {
            continuation?.resume(throwing: SignInError.unexpectedCredential)
            continuation = nil
            return
        }
        guard let tokenData = credential.identityToken,
              let identityToken = String(data: tokenData, encoding: .utf8) else {
            continuation?.resume(throwing: SignInError.missingIdentityToken)
            continuation = nil
            return
        }
        let code = credential.authorizationCode.flatMap { String(data: $0, encoding: .utf8) }
        // Apple returns the name on the FIRST authorization only. A nil here is
        // the normal case on every later sign-in and on every extra device.
        let name = credential.fullName.flatMap {
            PersonNameComponentsFormatter().string(from: $0).trimmingCharacters(in: .whitespaces)
        }
        continuation?.resume(returning: AppleProof(
            identityToken: identityToken,
            authorizationCode: code,
            userIdentifier: credential.user,
            fullName: (name?.isEmpty ?? true) ? nil : name
        ))
        continuation = nil
    }

    public func authorizationController(
        controller: ASAuthorizationController,
        didCompleteWithError error: any Error
    ) {
        continuation?.resume(throwing: error)
        continuation = nil
    }
}

extension SystemAppleSignInClient: ASAuthorizationControllerPresentationContextProviding {
    public func presentationAnchor(for controller: ASAuthorizationController) -> ASPresentationAnchor {
        let scene = UIApplication.shared.connectedScenes
            .compactMap { $0 as? UIWindowScene }
            .first { $0.activationState == .foregroundActive }
        return scene?.keyWindow ?? ASPresentationAnchor()
    }
}

/// Asks Apple whether a stored credential still authorizes us.
public struct SystemAppleCredentialStateClient: AppleCredentialStateClient {
    public init() {}

    public func credentialState(forUserIdentifier userIdentifier: String) async throws -> AppleCredentialState {
        let state = try await ASAuthorizationAppleIDProvider().credentialState(forUserID: userIdentifier)
        return switch state {
        case .authorized: .authorized
        case .revoked: .revoked
        case .notFound: .notFound
        case .transferred: .transferred
        @unknown default: .notFound
        }
    }
}
#endif
