import Foundation
import CryptoKit
import BabyCore

/// Validated derivative of supplementary S1/S2/S5/S6; never provides head rows.
/// Any malformed row invalidates the entire source rather than exposing a partial table.
struct SwissGrowthReference: Sendable {
    static let months = Array(0...24) + [30, 36, 42, 48, 54, 60]
    private static let printedYears = [
        "0", "0.083", "0.167", "0.25", "0.333", "0.417", "0.5", "0.583", "0.667", "0.75",
        "0.833", "0.917", "1", "1.083", "1.167", "1.25", "1.333", "1.417", "1.5", "1.583",
        "1.667", "1.75", "1.833", "1.917", "2", "2.5", "3", "3.5", "4", "4.5", "5"
    ]
    private static let bundled: SwissGrowthReference = {
        let data = Bundle.module.url(forResource: "SwissPEZZ2025", withExtension: "csv")
            .flatMap { try? Data(contentsOf: $0) }
        // Pinned to the reviewed derivative, also recorded in its bundled manifest.
        return SwissGrowthReference(data: data,
            expectedSHA256: "5f01d540dcab723a8a8b017bdbc639f53186f333d5c78a56cd02e47fb61de926")
    }()

    private let rows: [String: [Int: GrowthLMSRow]]

    init() { self = Self.bundled }

    init(data: Data?, expectedSHA256: String?) {
        guard let data else { self.init(csv: nil); return }
        if let expectedSHA256 {
            let hash = SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
            guard hash == expectedSHA256 else { self.init(csv: nil); return }
        }
        self.init(csv: String(data: data, encoding: .utf8))
    }

    init(csv: String?) { self.rows = Self.decode(csv) ?? [:] }

    func row(metric: GrowthMetric, sex: GrowthReferenceSex, month: Int) -> GrowthLMSRow? {
        guard metric != .headCircumference else { return nil }
        return rows[metric.rawValue + ":" + sex.rawValue]?[month]
    }

    private static func decode(_ csv: String?) -> [String: [Int: GrowthLMSRow]]? {
        guard let csv else { return nil }
        var lines = csv.split(separator: "\n", omittingEmptySubsequences: false)
        if lines.last?.isEmpty == true { lines.removeLast() }
        guard lines.count == 125,
              lines[0] == "metric,sex,month,printedYears,p3,p10,p25,m,p75,p90,p97,l,s" else { return nil }
        let tables: [(GrowthMetric, GrowthReferenceSex)] = [(.lengthHeight, .girls), (.lengthHeight, .boys),
                                                           (.weight, .girls), (.weight, .boys)]
        let normalZ = [-1.8807936081512509, -1.2815515655446004, -0.6744897501960817,
                       0, 0.6744897501960817, 1.2815515655446004, 1.8807936081512509]
        var rows: [String: [Int: GrowthLMSRow]] = [:]
        for (index, line) in lines.dropFirst().enumerated() {
            let fields = line.split(separator: ",", omittingEmptySubsequences: false)
            let (metric, sex) = tables[index / months.count]
            let nodeIndex = index % months.count
            guard fields.count == 13, fields[0] == metric.rawValue, fields[1] == sex.rawValue,
                  Int(fields[2]) == months[nodeIndex], fields[3] == Substring(printedYears[nodeIndex]),
                  let age = Double(fields[3]), age.isFinite, age >= 0,
                  let l = Double(fields[11]), let s = Double(fields[12]) else { return nil }
            let centiles = fields[4...10].compactMap { Double($0) }
            guard centiles.count == 7, centiles.allSatisfy({ $0.isFinite && $0 > 0 }),
                  zip(centiles, centiles.dropFirst()).allSatisfy({ $0 < $1 }) else { return nil }
            let row = GrowthLMSRow(l: l, m: centiles[3], s: s)
            guard row.isValid else { return nil }
            let tolerance = metric == .weight ? 0.003 : 0.01
            for (z, published) in zip(normalZ, centiles) {
                guard let derived = row.quantile(z), abs(derived - published) <= tolerance else { return nil }
            }
            rows[metric.rawValue + ":" + sex.rawValue, default: [:]][months[nodeIndex]] = row
        }
        return rows
    }
}
