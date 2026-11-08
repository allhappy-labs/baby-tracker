import Foundation
import Observation
import BabyCore
import BabyStore

/// The baby's vaccinations for the Insights card and the screen it opens.
/// All time: it ignores the range picker.
@MainActor @Observable
public final class ImmunizationsViewModel {
    private let store: BabyStore
    private let babyID: UUID
    private(set) var presentation = ImmunizationsPresentation.empty

    public init(store: BabyStore, babyID: UUID) {
        self.store = store
        self.babyID = babyID
    }

    public func load() throws {
        do {
            let baby = try store.baby(babyID)
            let birthDay = baby?.localBirthDate ?? baby.flatMap { try? LocalDate(displaying: $0.birthDate, in: .current) }
            let shots = try store.immunizations(for: babyID).map {
                ImmunizationShot(id: $0.id, vaccine: $0.vaccine, dose: $0.dose, givenAt: $0.givenAt, timeZoneID: $0.timeZoneID)
            }
            presentation = ImmunizationsPresentation(shots: shots, birthDay: birthDay)
        } catch {
            // A realm switch or failed read must not leave another family's shots on screen.
            presentation = .empty
            throw error
        }
    }

    /// Clears what is shown, when the Insights load it belongs to failed.
    func reset() { presentation = .empty }
}
