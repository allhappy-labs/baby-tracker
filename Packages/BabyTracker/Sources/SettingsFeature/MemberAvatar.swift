import BabySync
import BabyUI
import SwiftUI

/// A circle with a member's initials, in a colour that is theirs.
///
/// The colour comes from the member's id, not their name or position, so a
/// rename or a new member joining never repaints anyone. Decorative for
/// VoiceOver: the name is always read from the row beside it.
struct MemberAvatar: View {
    let member: FamilyMember
    private let base: CGFloat
    @ScaledMetric private var scaled: CGFloat

    init(member: FamilyMember, diameter: CGFloat = 36) {
        self.member = member
        base = diameter
        _scaled = ScaledMetric(wrappedValue: diameter, relativeTo: .body)
    }

    /// Grows with the text, but only so far: at the accessibility sizes an
    /// uncapped circle took a third of the row and hyphenated the name
    /// beside it ("Grand-ma"). The name is what matters; the circle is a cue.
    private var diameter: CGFloat { min(scaled, base * 1.5) }

    var body: some View {
        Text(MemberAvatarStyle.initials(of: member.displayName))
            .font(.system(.body, design: .rounded, weight: .semibold))
            .minimumScaleFactor(0.5)
            .lineLimit(1)
            .padding(4)
            .foregroundStyle(Palette.ground)
            .frame(width: diameter, height: diameter)
            .background(MemberAvatarStyle.colour(for: member.id), in: Circle())
            .accessibilityHidden(true)
    }
}

/// What an avatar shows, apart from the view that draws it.
enum MemberAvatarStyle {
    /// The palette's own light tones, so dark initials read on every one.
    private static let colours: [Color] = [Palette.feed, Palette.sleep, Palette.diaper, Palette.primaryText]

    /// Stable across launches — `hashValue` is not, it is seeded per process.
    static func colourIndex(for id: UUID) -> Int {
        let bytes = withUnsafeBytes(of: id.uuid) { Array($0) }
        return bytes.reduce(0) { ($0 &* 31 &+ Int($1)) & 0xFFFF } % colours.count
    }

    static func colour(for id: UUID) -> Color { colours[colourIndex(for: id)] }

    /// The first letter of the first two words: "Olena" → "O", "Grandma Rose"
    /// → "GR". A name with no letters at all still gets a mark.
    static func initials(of name: String) -> String {
        let words = name.split(whereSeparator: \.isWhitespace).prefix(2)
        let letters = words.compactMap(\.first).map { String($0).uppercased() }.joined()
        return letters.isEmpty ? "?" : letters
    }
}
