import SwiftUI
import BabyUI

/// Shown when the store will not open. Nothing else in the app works, so there
/// is nothing to return to. Offers retry only — deliberately NO action that
/// deletes or resets the store, and the file on disk is never touched.
struct StoreFailureView: View {
    let message: String
    let onRetry: () -> Void

    var body: some View {
        VStack(spacing: 18) {
            Image(systemName: "exclamationmark.triangle")
                .font(.largeTitle)
                .foregroundStyle(Palette.feed)

            Text(message)
                .font(.title3.weight(.semibold))
                .multilineTextAlignment(.center)

            Text(StoreFailureCopy.reassurance)
                .font(.subheadline)
                .foregroundStyle(Palette.secondaryText)
                .multilineTextAlignment(.center)

            Button(StoreFailureCopy.retry, action: onRetry)
                .buttonStyle(.borderedProminent)
                .tint(Palette.feed)
                .frame(minHeight: 44)
        }
        .padding(28)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Palette.ground.ignoresSafeArea())
        .accessibilityElement(children: .contain)
    }
}
