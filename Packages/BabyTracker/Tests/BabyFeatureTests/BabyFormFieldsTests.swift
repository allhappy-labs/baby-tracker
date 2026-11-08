import Testing
import SwiftUI
import BabyCore
@testable import BabyFeature

@MainActor
private func height(of view: some View, width: CGFloat) throws -> Int {
    let framed = view.environment(\.colorScheme, .dark).frame(width: width)
    return try #require(ImageRenderer(content: framed).cgImage).height
}

@MainActor
private func fields(name: String, hasDueDate: Bool = true) -> some View {
    BabyFormFields(
        name: .constant(name),
        birthDate: .constant(Date(timeIntervalSince1970: 1_700_000_000)),
        referenceSex: .constant(.girls),
        hasDueDate: .constant(hasDueDate),
        dueDate: .constant(Date(timeIntervalSince1970: 1_700_000_000)),
        identifierPrefix: "addBaby"
    )
}

/// Renders every field. macOS ImageRenderer does not scale text with Dynamic
/// Type (ruling R7), so a narrow width stands in for the larger text: the
/// labels wrap by the word rather than being cut, and the fields grow taller.
@MainActor
@Test func babyFormFieldsRendersAllFields() throws {
    let withDueDate = try height(of: fields(name: "Мірослава"), width: 400)
    let withoutDueDate = try height(of: fields(name: "Мірослава", hasDueDate: false), width: 400)
    #expect(withDueDate > withoutDueDate, "the due date field did not appear with its toggle on")

    let view = fields(name: "Мірослава")
    #expect(try height(of: view, width: 120) > height(of: view, width: 400))
}

@Test func babyFormFieldsIdentifiersKeepEachSheetsPrefix() {
    #expect(BabyFormFields.identifier("referenceSex", prefix: "addBaby") == "addBaby.referenceSex")
    #expect(BabyFormFields.identifier("hasDueDate", prefix: "baby") == "baby.hasDueDate")
    #expect(BabyFormFields.identifier("dueDate", prefix: "baby") == "baby.dueDate")
}
