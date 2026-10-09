import Foundation
import BabyCore

/// Compact feeding summaries shared by Home and History.
public enum FeedSummary {
    public static func sessionOverview(
        volumeML: Double,
        activeSeconds: Int,
        unit: VolumeUnit,
        activeFormat: String,
        feedCountText: String
    ) -> String {
        var parts: [String] = []
        if volumeML > 0 {
            parts.append(Volume(millilitres: volumeML).formatted(as: unit))
        }
        if activeSeconds > 0 {
            parts.append(String(format: activeFormat, locale: .current,
                                ElapsedFormatter.total(activeSeconds)))
        }
        parts.append(feedCountText)
        return parts.joined(separator: " · ")
    }

    public static func componentOverview(
        _ components: [FeedComponent],
        breastSeconds: Int,
        bottleVolumeML: Double,
        unit: VolumeUnit,
        breastFormat: String,
        bottleFormat: String,
        solidsTitle: String
    ) -> String {
        let bottleSeconds = components.compactMap(\.bottleSeconds).reduce(0, +)
        if components.allSatisfy({ $0.bottleContents != nil }) {
            var contents: [BottleContents] = []
            for component in components {
                if let kind = component.bottleContents, !contents.contains(kind) {
                    contents.append(kind)
                }
            }
            var parts = [Volume(millilitres: bottleVolumeML).formatted(as: unit),
                         contents.map(\.displayName).joined(separator: " + ")]
            if bottleSeconds > 0 { parts.append(ElapsedFormatter.precise(bottleSeconds)) }
            return parts.joined(separator: " · ")
        }

        var parts: [String] = []
        if breastSeconds > 0 {
            parts.append(String(format: breastFormat, locale: .current, ElapsedFormatter.precise(breastSeconds)))
        }
        if bottleVolumeML > 0 {
            parts.append(String(format: bottleFormat, locale: .current,
                                Volume(millilitres: bottleVolumeML).formatted(as: unit)))
            if bottleSeconds > 0 { parts.append(ElapsedFormatter.precise(bottleSeconds)) }
        }
        if components.contains(where: { if case .solids = $0 { true } else { false } }) {
            parts.append(solidsTitle)
        }
        return parts.isEmpty ? "—" : parts.joined(separator: " · ")
    }
}
