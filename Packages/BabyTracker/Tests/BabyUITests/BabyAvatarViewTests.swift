import Testing
import BabyCore
@testable import BabyUI
#if canImport(UIKit)
import UIKit
#endif

@Test func everyAvatarHasAnAssetAndADistinctTint() {
    #expect(Set(BabyAvatar.allCases.map(\.assetName)).count == BabyAvatar.allCases.count)
    #if canImport(UIKit)
    for avatar in BabyAvatar.allCases {
        #expect(UIImage(named: avatar.assetName, in: .module, with: nil) != nil, "\(avatar) asset missing")
    }
    #endif
    let tints = BabyAvatar.allCases.filter { $0 != .neutral }.map { "\($0.tint)" }
    #expect(Set(tints).count == 8)
}
