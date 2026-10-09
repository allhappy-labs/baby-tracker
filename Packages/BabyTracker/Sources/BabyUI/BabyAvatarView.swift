import SwiftUI
import BabyCore

public extension BabyAvatar {
    var assetName: String { "avatar-" + rawValue.replacingOccurrences(of: ".", with: "-") }

    var tint: Color {
        switch self {
        case .boy1: Palette.avatarBoy1
        case .boy2: Palette.avatarBoy2
        case .boy3: Palette.avatarBoy3
        case .boy4: Palette.avatarBoy4
        case .girl1: Palette.avatarGirl1
        case .girl2: Palette.avatarGirl2
        case .girl3: Palette.avatarGirl3
        case .girl4: Palette.avatarGirl4
        case .neutral: Palette.secondaryText
        }
    }
}

/// A baby's face, tinted with its own colour. Hidden from VoiceOver: the
/// name beside it is what identifies the baby.
public struct BabyAvatarView: View {
    private let avatar: BabyAvatar
    private let size: CGFloat

    public init(_ avatar: BabyAvatar, size: CGFloat) {
        self.avatar = avatar
        self.size = size
    }

    public var body: some View {
        Image(avatar.assetName, bundle: .module)
            .renderingMode(.template)
            .resizable()
            .scaledToFit()
            .frame(width: size, height: size)
            .foregroundStyle(avatar.tint)
            .accessibilityHidden(true)
    }
}
