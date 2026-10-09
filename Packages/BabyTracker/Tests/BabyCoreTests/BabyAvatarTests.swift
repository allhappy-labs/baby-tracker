import Testing
@testable import BabyCore

struct BabyAvatarTests {
    @Test func storedIDsAreExactlyTheEight() {
        #expect(BabyAvatar.storedIDs == ["boy.1", "boy.2", "boy.3", "boy.4", "girl.1", "girl.2", "girl.3", "girl.4"])
    }

    @Test func unknownIDsDrawNeutral() {
        #expect(BabyAvatar(iconID: nil) == .neutral)
        #expect(BabyAvatar(iconID: "") == .neutral)
        #expect(BabyAvatar(iconID: "neutral") == .neutral)
        #expect(BabyAvatar(iconID: "boy.9") == .neutral)
        #expect(BabyAvatar(iconID: "girl.3") == .girl3)
    }

    @Test func firstBabyGetsTheFirstIconOfTheirSet() {
        #expect(BabyAvatar.assign(sex: .boys, siblings: []) == "boy.1")
        #expect(BabyAvatar.assign(sex: .girls, siblings: ["boy.1"]) == "girl.1")
    }

    @Test func twinsGetDifferentIcons() {
        #expect(BabyAvatar.assign(sex: .boys, siblings: ["boy.1"]) == "boy.2")
    }

    @Test func aGapIsFilledFirst() {
        #expect(BabyAvatar.assign(sex: .boys, siblings: ["boy.1", "boy.3", nil, "girl.2"]) == "boy.2")
    }

    @Test func aFifthSameSexBabyReusesTheLeastUsedLowestNumber() {
        #expect(BabyAvatar.assign(sex: .girls, siblings: ["girl.1", "girl.2", "girl.3", "girl.4"]) == "girl.1")
        #expect(BabyAvatar.assign(sex: .girls, siblings: ["girl.1", "girl.2", "girl.3", "girl.4", "girl.1"]) == "girl.2")
    }

    @Test func otherSetAndUnknownIDsAreIgnored() {
        #expect(BabyAvatar.assign(sex: .boys, siblings: ["girl.1", "girl.2", "bogus", nil]) == "boy.1")
    }
}
