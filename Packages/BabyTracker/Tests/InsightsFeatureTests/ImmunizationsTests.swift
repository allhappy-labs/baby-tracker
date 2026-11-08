import Foundation
import Testing
import BabyCore
@testable import BabyStore
@testable import InsightsFeature

@MainActor @Suite("Immunizations card and screen")
struct ImmunizationsTests {
    static let locale = Locale(identifier: "en_US")
    static let zone = "UTC"
    /// 2026-09-03 12:00 UTC.
    static let september3 = Date(timeIntervalSince1970: 1_788_436_800)
    static func daysBefore(_ days: Double, _ date: Date = september3) -> Date { date.addingTimeInterval(-days * 86_400) }
    static func birth(daysBefore days: Double) throws -> LocalDate {
        try LocalDate(displaying: daysBefore(days), in: TimeZone(identifier: zone)!)
    }
    static func shot(_ vaccine: String, _ dose: Int?, _ givenAt: Date) -> ImmunizationShot {
        ImmunizationShot(id: UUID(), vaccine: vaccine, dose: dose, givenAt: givenAt, timeZoneID: zone)
    }

    @Test func noShotsIsEmpty() throws {
        let p = ImmunizationsPresentation(shots: [], birthDay: try Self.birth(daysBefore: 120), locale: Self.locale)
        #expect(p.isEmpty)
        #expect(p.count == 0)
        #expect(p.latestText == nil)
        #expect(p.sections.isEmpty)
    }

    @Test func theCardNamesTheLatestShot() throws {
        let p = ImmunizationsPresentation(shots: [
            Self.shot("Prevenar 13", 2, Self.september3),
            Self.shot("Infanrix hexa", 1, Self.daysBefore(60)),
        ], birthDay: try Self.birth(daysBefore: 120), locale: Self.locale)
        #expect(p.count == 2)
        #expect(p.latestText == "Prevenar 13 · dose 2 · Sep 3")
        let noDose = ImmunizationsPresentation(shots: [Self.shot("MMR", nil, Self.september3)], birthDay: nil, locale: Self.locale)
        #expect(noDose.latestText == "MMR · Sep 3")
    }

    @Test func sectionsAreByVaccineNewestFirstRowsOldestFirst() throws {
        let p = ImmunizationsPresentation(shots: [
            Self.shot("Prevenar 13", 2, Self.september3),
            Self.shot("Infanrix hexa", 2, Self.daysBefore(1)),
            Self.shot("Infanrix hexa", 1, Self.daysBefore(61)),
            Self.shot("prevenar 13", 1, Self.daysBefore(62)),
        ], birthDay: try Self.birth(daysBefore: 122), locale: Self.locale)
        // The heading is the spelling of the vaccine's earliest shot, as the picker shows it.
        #expect(p.sections.map(\.vaccine) == ["prevenar 13", "Infanrix hexa"])
        #expect(p.sections[0].rows.map(\.text) == ["Dose 1 · Jul 3, 2026 · 8w", "Dose 2 · Sep 3, 2026 · 3mo"])
        #expect(p.sections[1].rows.count == 2)
    }

    @Test func sectionsUseTheStoresNameKey() throws {
        let shots = [Self.shot("Prevenar 13", 2, Self.september3), Self.shot("PREVENAR 13", 1, Self.daysBefore(60))]
        let p = ImmunizationsPresentation(shots: shots, birthDay: nil, locale: Self.locale)
        #expect(p.sections.count == 1)
        #expect(Set(shots.map { BabyStore.vaccineNameKey($0.vaccine) }).count == 1)
    }

    @Test(arguments: [(10.0, "10d"), (13.0, "13d"), (14.0, "2w"), (90.0, "12w"), (122.0, "3mo"), (800.0, "2y")])
    func ageUsesGrowthsShortForm(_ days: Double, _ age: String) throws {
        let p = ImmunizationsPresentation(shots: [Self.shot("MMR", nil, Self.september3)],
                                          birthDay: try Self.birth(daysBefore: days), locale: Self.locale)
        #expect(p.sections[0].rows[0].text == "Sep 3, 2026 · \(age)")
    }

    @Test func aRowWithoutABirthDayOrBeforeItShowsNoAge() throws {
        let unknown = ImmunizationsPresentation(shots: [Self.shot("MMR", 1, Self.september3)], birthDay: nil, locale: Self.locale)
        #expect(unknown.sections[0].rows[0].text == "Dose 1 · Sep 3, 2026")
        let before = ImmunizationsPresentation(shots: [Self.shot("MMR", 1, Self.september3)],
                                               birthDay: try Self.birth(daysBefore: -5), locale: Self.locale)
        #expect(before.sections[0].rows[0].text == "Dose 1 · Sep 3, 2026")
        let birthday = ImmunizationsPresentation(shots: [Self.shot("Hep B", 1, Self.september3)],
                                                 birthDay: try Self.birth(daysBefore: 0), locale: Self.locale)
        #expect(birthday.sections[0].rows[0].text == "Dose 1 · Sep 3, 2026 · 0d")
    }

    @Test func theModelReadsTheBabysShotsAndFollowsTheStore() throws {
        let clock = TestDateProvider(Self.september3)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let mira = try store.addBaby(name: "Mira", birthDate: Self.daysBefore(122), referenceSex: .girls, expectedDueDate: nil)
        let noah = try store.addBaby(name: "Noah", birthDate: Self.daysBefore(400), referenceSex: .boys, expectedDueDate: nil)
        try store.logImmunization(vaccine: "MMR", dose: 1, for: noah.id)
        let model = ImmunizationsViewModel(store: store, babyID: mira.id)
        try model.load()
        #expect(model.presentation.isEmpty)
        let shot = try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: mira.id)
        try model.load()
        #expect(model.presentation.count == 1)
        #expect(model.presentation.sections[0].rows[0].id == shot.id)
    }

    // Review Focus 5.
    @Test func aDeletedShotLeavesTheScreen() throws {
        let clock = TestDateProvider(Self.september3)
        let store = try BabyStore.inMemory(dateProvider: clock)
        let mira = try store.addBaby(name: "Mira", birthDate: Self.daysBefore(122), referenceSex: .girls, expectedDueDate: nil)
        let shot = try store.logImmunization(vaccine: "Prevenar 13", dose: 1, for: mira.id)
        let insights = InsightsViewModel(store: store, babyID: mira.id)
        try insights.load()
        #expect(insights.immunizations.presentation.count == 1)
        try store.softDelete(shot)
        try insights.load()
        #expect(insights.immunizations.presentation.isEmpty)
    }
}
