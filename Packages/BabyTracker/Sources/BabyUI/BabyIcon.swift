import SwiftUI

/// The app's own line-style glyph set, replacing SF Symbols for every
/// tracker, breast side and diaper content. Each case names a vector asset in
/// `Icons.xcassets`, rendered as a template image so it tints like a symbol.
public enum BabyIcon: String, CaseIterable, Sendable {
    case feed, breastLeft, breastRight, bottle, sleep, diaper, solids, meds, growth, wet, dirty, mixed, stop, pause, more, clock, plus, close, vaccine, pump

    /// The asset catalog name backing this icon. `breastLeft` and
    /// `breastRight` share the feed drop — the letter overlay in
    /// `BabyIconView` is what tells them apart.
    public var assetName: String {
        switch self {
        case .breastLeft, .breastRight: "icon-feed"
        default: "icon-" + rawValue
        }
    }

    /// The localized letter `BabyIconView` overlays on a breast side's drop.
    var letterKey: String? {
        switch self {
        case .breastLeft: "L"
        case .breastRight: "R"
        default: nil
        }
    }

    /// The localized side letter — "L" or "R" — for a label that names a
    /// breast side beside or instead of the glyph. Nil for every other icon.
    public var letter: String? {
        letterKey.map { String(localized: String.LocalizationValue($0), bundle: .module) }
    }
}

/// A single tracker glyph, rendered as a template image so `.foregroundStyle`
/// tints it the way `Image(systemName:)` used to.
public struct BabyIconView: View {
    private let icon: BabyIcon
    private let size: CGFloat

    public init(_ icon: BabyIcon, size: CGFloat = 22) {
        self.icon = icon
        self.size = size
    }

    public var body: some View {
        Image(icon.assetName, bundle: .module)
            .renderingMode(.template)
            .resizable()
            .scaledToFit()
            .frame(width: size, height: size)
            .overlay {
                if let letter = icon.letter {
                    // Drawn, not laid out as a `Text`: the letter is part of
                    // the glyph and sized with it. As a `Text` it surfaced to
                    // the accessibility audit as a fixed-size "L" wherever
                    // the glyph sat inside an element that ignores its
                    // children (the hero, the parts row, the join chip), and
                    // was reported as text that ignores Dynamic Type.
                    Canvas { context, canvas in
                        let text = context.resolve(
                            Text(letter).font(.system(size: size * 0.42, weight: .heavy, design: .rounded))
                        )
                        context.draw(text, at: CGPoint(x: canvas.width / 2, y: canvas.height / 2 + size * 0.12))
                    }
                }
            }
            .accessibilityHidden(true)
    }

    /// Wet is a blue drop, dirty a brown dot, mixed shows both side by side.
    /// The single `icon-mixed` asset (both shown as one flat glyph) is for
    /// monochrome contexts, such as a widget's accent colour, where two
    /// tinted glyphs cannot be composited.
    @ViewBuilder
    public static func diaperContents(_ contents: DiaperContentsMark, size: CGFloat = 20) -> some View {
        HStack(spacing: 0) {
            if contents != .dirty {
                BabyIconView(.wet, size: size).foregroundStyle(Palette.wet)
            }
            if contents != .wet {
                BabyIconView(.dirty, size: size).foregroundStyle(Palette.dirty)
            }
        }
    }
}

/// What a diaper's contents glyph shows. Distinct from `BabyCore`'s
/// `DiaperContents`, which also carries `.dry` — a case the two-tap fast path
/// never offers and this glyph never needs to draw.
public enum DiaperContentsMark: Sendable, Equatable { case wet, dirty, mixed }
