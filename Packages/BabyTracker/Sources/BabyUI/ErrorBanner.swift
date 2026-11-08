import SwiftUI

/// Every user-facing failure string in the app. Factual, never evaluative,
/// never blaming — a parent who just lost a log entry is not also told off.
public enum ErrorBannerCopy {
    public static func saveFailed(what: String) -> String {
        String(format: String(localized: "Couldn't save that %@", bundle: .module), what)
    }

    public static func deleteFailed(what: String) -> String {
        String(format: String(localized: "Couldn't delete that %@", bundle: .module), what)
    }

    public static let loadFailed = String(localized: "Couldn't load the latest entries", bundle: .module)

    /// The dismiss control's VoiceOver label — the only route to clearing a
    /// banner once the user has dealt with it.
    public static let dismissLabel = String(localized: "Dismiss", bundle: .module)
}

/// The nouns `ErrorBannerCopy` is built from. They live here rather than at the
/// call site so no view hard-codes a user-visible string, and so the same thing
/// is called the same thing everywhere it can fail.
public enum ErrorSubject {
    public static let diaperChange = String(localized: "diaper change", bundle: .module)
    public static let feed = String(localized: "feed", bundle: .module)
    public static let bottle = String(localized: "bottle", bundle: .module)
    public static let solids = String(localized: "solids", bundle: .module)
    public static let sleep = String(localized: "sleep", bundle: .module)
    public static let baby = String(localized: "baby", bundle: .module)
    public static let entry = String(localized: "entry", bundle: .module)
    public static let supplement = String(localized: "med", bundle: .module)
    public static let vaccination = String(localized: "vaccination", bundle: .module)
    public static let pump = String(localized: "pumping session", bundle: .module)
}

/// Non-blocking. Sits above content, below the navigation bar, and leaves
/// everything beneath it tappable — the thing the user was logging is often
/// still happening.
public struct ErrorBanner: View {
    private let message: String
    private let onDismiss: () -> Void

    public init(message: String, onDismiss: @escaping () -> Void) {
        self.message = message
        self.onDismiss = onDismiss
    }

    public var body: some View {
        HStack(spacing: 12) {
            // The icon and the message combine into one element so VoiceOver
            // reads the sentence rather than "warning" then the text.
            HStack(spacing: 12) {
                Image(systemName: "exclamationmark.triangle.fill")
                    .foregroundStyle(Palette.feed)

                Text(message)
                    .font(.subheadline.weight(.medium))
                    .foregroundStyle(Palette.primaryText)
                    // 44pt, matching the dismiss button beside it. This
                    // element is actionable — the banner publishes a "Dismiss"
                    // accessibility action and the rotor reaches it through
                    // here — so it is a hit target, and a hit target the
                    // height of one line of subheadline text is one you have
                    // to aim at. The banner's own height does not change: the
                    // 44pt dismiss button already set it.
                    //
                    // On the Text and not on the HStack below, because
                    // `.combine` derives the element's frame from the union of
                    // its CHILDREN — a minHeight on the container it is
                    // applied to is not what gets measured.
                    .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
            }
            .accessibilityElement(children: .combine)
            // Belt and braces: states the combined element's shape outright
            // rather than leaving it to be inferred from the children.
            .contentShape(.accessibility, Rectangle())
            .accessibilityLabel(message)

            // Deliberately OUTSIDE that combine. Combining the whole banner
            // absorbed this button into the label, and manual dismissal is the
            // only way to clear a banner the user has already dealt with — so a
            // VoiceOver user could be left with no route to it. It stays its
            // own focusable, actionable element, and carries the action on the
            // message element too so the rotor reaches it either way.
            Button(action: onDismiss) {
                Image(systemName: "xmark")
                    .font(.footnote.weight(.bold))
                    .frame(width: 44, height: 44)
            }
            .buttonStyle(.plain)
            .contentShape(Rectangle())
            .foregroundStyle(Palette.secondaryText)
            .accessibilityLabel(ErrorBannerCopy.dismissLabel)
            .accessibilityAddTraits(.isButton)
        }
        .padding(.leading, 16)
        .padding(.vertical, 4)
        .background(.white.opacity(0.08), in: .rect(cornerRadius: 16))
        .padding(.horizontal, 12)
        .accessibilityElement(children: .contain)
        .accessibilityAction(named: Text(ErrorBannerCopy.dismissLabel), onDismiss)
    }
}
