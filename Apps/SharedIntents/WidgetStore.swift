import Foundation
import BabyCore
import BabyStore
import BabyFeature

@MainActor
enum WidgetStore {
    /// One container per process. The widget extension is short-lived, so this
    /// is created on demand rather than held by a long-lived owner.
    ///
    /// Never moves a file: a store another schema wrote throws, and every
    /// caller treats a throw as "unavailable". What happens to that store is
    /// the app's decision, made the next time it opens.
    static func make() throws -> BabyStore {
        let defaults = UserDefaults(suiteName: BabySchema.appGroupID) ?? .standard
        return BabyStore(
            container: try BabySchema.container(inMemory: false, ifIncompatible: .refuse),
            dateProvider: SystemDateProvider(),
            deviceID: defaults.string(forKey: "deviceID") ?? "widget"
        )
    }

    static func currentBaby(in store: BabyStore) throws -> Baby? {
        SelectedBabyStore().resolve(from: try store.babies())
    }

    /// A configured widget never falls through to the app's selected baby.
    static func baby(_ id: String, in store: BabyStore) throws -> Baby? {
        guard let uuid = UUID(uuidString: id) else { return nil }
        return try store.baby(uuid)
    }

    /// The baby an intent should act on: the one its Live Activity named, or
    /// the selected one when no activity named any. See `ActivityTarget`.
    static func target(_ activityBabyID: String?, in store: BabyStore) throws -> Baby? {
        let babies = try store.babies()
        return ActivityTarget.resolve(
            activityBabyID: activityBabyID,
            among: babies,
            fallback: SelectedBabyStore().resolve(from: babies)
        )
    }
}
