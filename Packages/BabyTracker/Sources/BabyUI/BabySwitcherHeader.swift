import SwiftUI
import BabyCore

/// The baby's face and name, which is also the way to switch babies. One
/// view for Home, History and Insights so the three never drift apart again.
public struct BabySwitcherHeader: View {
    private let babyName: String
    private let avatar: BabyAvatar
    private let switcherID: String
    private let headingID: String
    private let onSwitchBaby: (() -> Void)?

    public init(babyName: String, avatar: BabyAvatar, switcherID: String, headingID: String,
                onSwitchBaby: (() -> Void)?) {
        self.babyName = babyName
        self.avatar = avatar
        self.switcherID = switcherID
        self.headingID = headingID
        self.onSwitchBaby = onSwitchBaby
    }

    public var body: some View {
        if let onSwitchBaby {
            Button(action: onSwitchBaby) {
                HStack(spacing: 8) {
                    label
                    Image(systemName: "chevron.down")
                        .font(.footnote.weight(.bold))
                        .foregroundStyle(Palette.secondaryText)
                }
                .frame(minHeight: 44)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(String(localized: "Switch baby", bundle: .module))
            .accessibilityValue(babyName)
            .accessibilityIdentifier(switcherID)
        } else if !babyName.isEmpty {
            label
                .frame(minHeight: 44)
                .accessibilityElement(children: .combine)
                .accessibilityAddTraits(.isHeader)
                .accessibilityIdentifier(headingID)
        }
    }

    private var label: some View {
        HStack(spacing: 8) {
            BabyAvatarView(avatar, size: 32)
            Text(babyName)
                .font(.title2.weight(.bold))
                .foregroundStyle(Palette.primaryText)
                .lineLimit(2)
                .minimumScaleFactor(0.8)
        }
    }
}
