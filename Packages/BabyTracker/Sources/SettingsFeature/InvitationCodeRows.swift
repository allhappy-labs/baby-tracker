import BabyCore
import BabyUI
import SwiftUI
#if canImport(UIKit)
import UIKit
#endif

/// The fallback below "Scan QR code": one text field and one button — paste
/// a bare code or a full link, then join. A camera was never the only way to
/// bring a code across.
///
/// Joins directly rather than through the "Join {family}?" step: the user
/// typed or pasted this themselves, on this screen, and pressed Join.
struct InvitationCodeRows<Model: FamilySyncSettingsModel>: View {
    let model: Model
    @Binding var isWorking: Bool
    @Environment(\.errorReporter) private var errorReporter

    @State private var invitationCode = ""
    @State private var joinProblem: String?

    var body: some View {
        TextField(FamilyCopy.invitationCodeField, text: $invitationCode, axis: .vertical)
            #if os(iOS)
            .textInputAutocapitalization(.never)
            #endif
            .autocorrectionDisabled()
            .frame(minHeight: 44)
            .accessibilityIdentifier("family.codeField")
            // A stale refusal next to a code the user has since changed
            // reads as if the new code was already tried and rejected.
            .onChange(of: invitationCode) { _, _ in joinProblem = nil }

        Button(FamilyCopy.pasteCode) {
            #if canImport(UIKit)
            if let pasted = UIPasteboard.general.string { invitationCode = pasted }
            #endif
        }
        .frame(minHeight: 44)
        .accessibilityIdentifier("family.pasteCode")

        Button(FamilyCopy.joinFamily) { join() }
            .frame(minHeight: 44)
            .disabled(
                invitationCode.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                    || !model.displayNameConfirmed
            )
            .accessibilityIdentifier("family.join")

        if let joinProblem {
            Text(joinProblem)
                .foregroundStyle(Palette.destructive)
                .accessibilityIdentifier("family.joinProblem")
        }
    }

    /// Joining gets its own inline error rather than the generic banner:
    /// a shape that never left the phone reads differently from a server's
    /// refusal, and both need to be seen right next to the field, not in a
    /// banner that a later action clears. It is still reported like every
    /// other failure, so a pattern of refused joins shows up in the logs.
    private func join() {
        isWorking = true
        Task {
            defer { isWorking = false }
            do {
                try await model.acceptInvitation(code: invitationCode)
                joinProblem = nil
                invitationCode = ""
                errorReporter.clear()
            } catch {
                let message = JoinFailureCopy.message(for: error)
                joinProblem = message
                errorReporter.report(message, error: error, category: AppLog.sync)
            }
        }
    }
}
