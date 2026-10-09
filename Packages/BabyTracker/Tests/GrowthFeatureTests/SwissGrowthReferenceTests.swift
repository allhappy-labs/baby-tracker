import Foundation
import Testing
import BabyCore
@testable import GrowthFeature

struct SwissGrowthReferenceTests {
    func bundledCSV() throws -> String {
        let url = try #require(Bundle.module.url(forResource: "SwissPEZZ2025", withExtension: "csv"))
        return try String(contentsOf: url, encoding: .utf8)
    }
    func rejects(_ csv: String?) {
        let reference = SwissGrowthReference(csv: csv)
        for metric in [GrowthMetric.weight, .lengthHeight] {
            for sex in [GrowthReferenceSex.girls, .boys] {
                #expect(reference.row(metric: metric, sex: sex, month: 0) == nil)
                #expect(reference.row(metric: metric, sex: sex, month: 60) == nil)
            }
        }
    }
    @Test func bundledSourceMapsAllFourTablesWithoutHeadRows() throws {
        let reference = SwissGrowthReference()
        for (metric, sex, median, l, s) in [
            (GrowthMetric.lengthHeight, GrowthReferenceSex.girls, 49.732, 0.8513, 0.0371),
            (.lengthHeight, .boys, 50.531, 0.5192, 0.0376),
            (.weight, .girls, 3.357, 0.3668, 0.1189),
            (.weight, .boys, 3.495, 0.4026, 0.1163)
        ] {
            let row = try #require(reference.row(metric: metric, sex: sex, month: 0))
            #expect(row.m == median && row.l == l && row.s == s)
            #expect(reference.row(metric: metric, sex: sex, month: 60) != nil)
            #expect(reference.row(metric: metric, sex: sex, month: 25) == nil)
            #expect(reference.row(metric: metric, sex: sex, month: -1) == nil)
            #expect(reference.row(metric: metric, sex: sex, month: 61) == nil)
            #expect(reference.row(metric: .headCircumference, sex: sex, month: 0) == nil)
        }
    }
    @Test func allPublishedNodesReconstructSevenCentilesAtPublicationTolerance() throws {
        let reference = SwissGrowthReference()
        let calculator = GrowthReferenceCalculator()
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(secondsFromGMT: 0)!
        let origin = calendar.date(from: DateComponents(year: 2020, month: 1, day: 1))!
        let normalZ = [-1.8807936081512509, -1.2815515655446004, -0.6744897501960817,
                       0, 0.6744897501960817, 1.2815515655446004, 1.8807936081512509]
        let ranks = [3.0, 10, 25, 50, 75, 90, 97]
        let lines = try bundledCSV().split(separator: "\n").dropFirst()
        #expect(lines.count == 124)
        for line in lines {
            let fields = line.split(separator: ",").map(String.init)
            let metric: GrowthMetric = fields[0] == "weight" ? .weight : .lengthHeight
            let sex: GrowthReferenceSex = fields[1] == "girls" ? .girls : .boys
            let month = try #require(Int(fields[2]))
            let row = try #require(reference.row(metric: metric, sex: sex, month: month))
            let at = calendar.date(byAdding: .month, value: month, to: origin)!
            let printed = fields[4...10].map { Double($0)! }
            let tolerance = metric == .weight ? 0.003 : 0.01
            #expect(row.m == printed[3])
            for index in normalZ.indices {
                // Independent inverse-LMS reconstruction of published table values.
                let z = normalZ[index]
                let value = row.l == 0 ? row.m * exp(row.s * z) : row.m * pow(1 + row.l * row.s * z, 1 / row.l)
                #expect(abs(value - printed[index]) <= tolerance)
                let output = calculator.result(metric: metric, value: value, position: month < 24 ? .lying : .standing,
                                               measuredAt: at, timeZoneID: "UTC", birthDate: try LocalDate("2020-01-01"),
                                               expectedDueDate: nil, referenceSex: sex)
                guard case let .available(p, m, low, high, corrected) = output else {
                    Issue.record("Unavailable source node: \(fields[0...2])"); continue
                }
                #expect(abs(p - ranks[index]) < 0.000001)
                #expect(m == printed[3] && !corrected)
                #expect(abs(low - printed[0]) <= tolerance && abs(high - printed[6]) <= tolerance)
            }
        }
    }
    @Test func missingEmptyAndIncorrectHeadersFailClosed() throws {
        rejects(nil)
        rejects("")
        let csv = try bundledCSV()
        rejects(csv.replacingOccurrences(of: "printedYears", with: "age"))
        rejects(csv.replacingOccurrences(of: "metric,sex", with: "sex,metric"))
        rejects(csv + "\n") // Only the generated single trailing newline is accepted.
    }
    @Test func missingDuplicateAndUnorderedRowsFailClosed() throws {
        let lines = try bundledCSV().split(separator: "\n").map(String.init)
        var missing = lines
        missing.remove(at: 2)
        rejects(missing.joined(separator: "\n") + "\n")
        var duplicate = lines
        duplicate[2] = duplicate[1]
        rejects(duplicate.joined(separator: "\n") + "\n")
        var unordered = lines
        unordered.swapAt(1, 2)
        rejects(unordered.joined(separator: "\n") + "\n")
        var groupSwap = lines
        groupSwap.swapAt(1, 32)
        rejects(groupSwap.joined(separator: "\n") + "\n")
    }
    @Test func malformedNumericFieldsAndUnsupportedMappingsFailClosed() throws {
        let lines = try bundledCSV().split(separator: "\n").map(String.init)
        for (column, values) in [
            (0, ["headCircumference", "unknown"]), (1, ["unknown"]), (2, ["-1", "1.5", "61"]),
            (3, ["0.083", "nan", "-1"]), (4, ["nan", "inf", "0", "-1"]),
            (7, ["0", "nan", "-1"]), (11, ["nan", "inf"]), (12, ["0", "-1", "nan", "inf"])
        ] {
            for value in values {
                var changed = lines
                var fields = changed[1].split(separator: ",").map(String.init)
                fields[column] = value
                changed[1] = fields.joined(separator: ",")
                rejects(changed.joined(separator: "\n") + "\n")
            }
        }
        rejects(lines.joined(separator: "\n").replacingOccurrences(of: "0.8513,0.0371", with: "0.8513"))
    }
    @Test func unorderedAndInconsistentCentilesFailClosed() throws {
        let csv = try bundledCSV()
        rejects(csv.replacingOccurrences(of: "46.284,47.379", with: "47.380,47.379"))
        rejects(csv.replacingOccurrences(of: "0.8513,0.0371", with: "0.8513,0.0500"))
        rejects(csv.replacingOccurrences(of: "46.284,47.379", with: "40.000,47.379"))
    }
    @Test func zeroAndNegativeLRemainValidParameters() throws {
        var lines = try bundledCSV().split(separator: "\n").map(String.init)
        // Independently rounded log-normal fixture, with L=0 and positive ordered centiles.
        lines[1] = "lengthHeight,girls,0,0,41.427,43.986,46.739,50.000,53.489,56.836,60.346,0.0000,0.1000"
        let reference = SwissGrowthReference(csv: lines.joined(separator: "\n") + "\n")
        #expect(try #require(reference.row(metric: .lengthHeight, sex: .girls, month: 0)).l == 0)
        #expect(try #require(reference.row(metric: .weight, sex: .girls, month: 60)).l == -0.9910)
    }
    @Test func invalidUTF8AndResourceChecksumMismatchFailClosed() throws {
        let invalid = SwissGrowthReference(data: Data([0xff, 0xfe]), expectedSHA256: nil)
        #expect(invalid.row(metric: .weight, sex: .girls, month: 0) == nil)
        let csv = try bundledCSV()
        let wrongHash = SwissGrowthReference(data: Data(csv.utf8), expectedSHA256: String(repeating: "0", count: 64))
        #expect(wrongHash.row(metric: .weight, sex: .girls, month: 0) == nil)
    }
}
