import Testing
import Foundation
@testable import BabyCore

@Test func ouncesRoundTripWithoutDrift() {
    let original = Volume(ounces: 4)
    let roundTripped = Volume(millilitres: original.millilitres)

    #expect(abs(roundTripped.ounces - 4) < 0.0001)
}

@Test func millilitresAreCanonical() {
    #expect(abs(Volume(ounces: 1).millilitres - 29.5735295625) < 0.0001)
}

@Test func formattingDropsPointlessDecimals() {
    #expect(Volume(millilitres: 120).formatted(as: .millilitres) == "120 ml")
    #expect(Volume(millilitres: 118.294).formatted(as: .ounces) == "4 oz")
    let separator = Locale.current.decimalSeparator ?? "."
    #expect(Volume(millilitres: 100).formatted(as: .ounces) == "3\(separator)4 oz")
}

@Test func eachUnitHasAReadableName() {
    #expect(VolumeUnit.millilitres.displayName == "Millilitres")
    #expect(VolumeUnit.ounces.displayName == "Ounces")
}
