import BabyStore
import BabySync
import BabyUI
import SwiftUI

/// One sync problem, in words a parent can act on.
///
/// Every issue carries a dismiss control. A banner that cannot be dismissed
/// becomes furniture the user learns to read past, and the next one — the one
/// that matters — goes unread with it.
public struct SyncIssueView: View {
    private let issue: SyncIssue
    private let onDismiss: () -> Void

    public init(issue: SyncIssue, onDismiss: @escaping () -> Void) {
        self.issue = issue
        self.onDismiss = onDismiss
    }

    public var body: some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: symbol)
                .foregroundStyle(issue.presentation.needsAttention ? Palette.destructive : Palette.secondaryText)
                .accessibilityHidden(true)

            Text(issue.presentation.message)
                .font(.footnote)
                .fixedSize(horizontal: false, vertical: true)

            Spacer(minLength: 0)

            Button(action: onDismiss) {
                Image(systemName: "xmark")
                    .font(.footnote.weight(.semibold))
                    .frame(minWidth: 44, minHeight: 44)
            }
            .accessibilityLabel(SyncIssueViewCopy.dismiss)
            .accessibilityIdentifier("family-sync-issue-dismiss-\(issue.id.uuidString)")
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 8)
        // One element, so VoiceOver reads the problem as a sentence rather
        // than stopping on an icon.
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("family-sync-issue-\(issue.id.uuidString)")
    }

    private var symbol: String {
        switch issue.presentation {
        case .retrying: "arrow.triangle.2.circlepath"
        case .revoked: "person.crop.circle.badge.xmark"
        case .timerCollision: "timer"
        case .importConflict: "questionmark.circle"
        case .deviceContinuityRequired: "person.badge.key"
        case .permanentRejection: "exclamationmark.triangle"
        }
    }
}

enum SyncIssueViewCopy {
    static let dismiss = String(localized: "Dismiss", bundle: .module)
}

/// The losing side of a timer race.
///
/// It says what happened and what was kept, and pointedly does not offer to
/// "retry" — the other caregiver's timer is the real one, and starting a
/// second would be two timers for one feed.
public struct TimerConflictView: View {
    private let message: String
    private let onAcknowledge: () -> Void

    public init(message: String = SyncIssueCopy.timerCollision, onAcknowledge: @escaping () -> Void) {
        self.message = message
        self.onAcknowledge = onAcknowledge
    }

    public var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text(TimerConflictCopy.title)
                .font(.headline)
                .accessibilityIdentifier("timer-conflict-title")

            Text(message)
                .font(.callout)
                .foregroundStyle(Palette.secondaryText)
                .fixedSize(horizontal: false, vertical: true)
                .accessibilityIdentifier("timer-conflict-message")

            Button(TimerConflictCopy.acknowledge, action: onAcknowledge)
                .frame(maxWidth: .infinity, minHeight: 44)
                .accessibilityIdentifier("timer-conflict-acknowledge")
        }
        .padding()
    }
}

enum TimerConflictCopy {
    static let title = String(localized: "Another caregiver already started this timer", bundle: .module)
    static let acknowledge = String(localized: "OK", bundle: .module)
}
