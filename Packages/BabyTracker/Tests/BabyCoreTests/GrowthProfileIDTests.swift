import Foundation
import Testing
@testable import BabyCore

struct GrowthProfileIDTests {
    @Test(arguments: [
        ("00000000-0000-0000-0000-000000000000", "052ae0b7-e763-5d48-bf4c-b283a15c071f"),
        ("123e4567-e89b-12d3-a456-426614174000", "101dea4a-277e-597f-a0a7-aedaadde0cde"),
        ("ffffffff-ffff-ffff-ffff-ffffffffffff", "5793676f-fb9d-5024-b24d-6a3748cd8085"),
    ])
    func matchesIndependentUUIDv5Vectors(baby: String, profile: String) throws {
        // Python uuid.uuid5(UUID(baby), "growth-profile-v1"); shared Swift/Go contract.
        let babyID = try #require(UUID(uuidString: baby))
        let expected = try #require(UUID(uuidString: profile))
        #expect(GrowthProfileID.forBaby(babyID) == expected)
    }

    @Test func preservesGrowthStorageValues() throws {
        #expect(GrowthMetric.allCases.map(\.rawValue) == ["weight", "lengthHeight", "headCircumference"])
        #expect(GrowthReferenceSex.allCases.map(\.rawValue) == ["girls", "boys"])
        #expect(LengthPosition.allCases.map(\.rawValue) == ["lying", "standing"])
        #expect(try JSONDecoder().decode(GrowthReferenceSex.self, from: Data("\"boys\"".utf8)) == .boys)
        #expect(try JSONEncoder().encode(LengthPosition.lying) == Data("\"lying\"".utf8))
        #expect(throws: DecodingError.self) {
            try JSONDecoder().decode(GrowthReferenceSex.self, from: Data("\"unspecified\"".utf8))
        }
    }
}
