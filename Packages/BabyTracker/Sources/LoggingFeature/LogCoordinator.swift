import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// Every sheet the dock, the ··· sheet, a widget tap, or a History row can
/// open.
public enum LogSheet: Identifiable {
    /// Start a timed bottle, or log what was drunk.
    case bottle
    /// A running bottle was just stopped and needs its amount.
    case bottleStopped(timerID: UUID)
    /// A bottle already logged — e.g. after a Live Activity's own Stop —
    /// needs its amount set after the fact.
    case bottleAmount(feedID: UUID)
    case solids
    case diaper
    case supplement
    /// Meds from the dock: the saved meds as chips.
    case meds
    case measurement
    /// Log a vaccination.
    case immunization
    /// The Pump Home action: start a session, or edit the one already running.
    case pump
    /// A pump just stopped needs its amount (How much?).
    case pumpAmount(pumpID: UUID)
    /// The ··· sheet: what the dock has no room for, and Past entry.
    case more
    case pastEntry
    /// One logged entry, in the sheet it was logged with, in edit mode —
    /// History's tap on a single row, or an Ask source.
    case editEntry(EntryRef)
    /// A feed or sleep of several parts, in the session sheet.
    case session(SessionKind, rootID: UUID)

    public var id: String {
        switch self {
        case .bottle: "bottle"
        // The timer/feed id is part of the identity, not decoration:
        // `.sheet(item:)` rebuilds only when the id changes, and a constant
        // id here would silently reuse a sheet already dialled to another
        // timer's volume.
        case .bottleStopped(let timerID): "bottleStopped-\(timerID)"
        case .bottleAmount(let feedID): "bottleAmount-\(feedID)"
        case .solids: "solids"
        case .measurement: "measurement"
        case .immunization: "immunization"
        case .pump: "pump"
        case .pumpAmount(let pumpID): "pumpAmount-\(pumpID)"
        case .supplement: "supplement"
        case .meds: "meds"
        case .diaper: "diaper"
        case .more: "more"
        case .pastEntry: "pastEntry"
        // The entry is part of the identity for the same reason as above: a
        // tap on another row must rebuild the sheet, not reuse this one.
        case .editEntry(let ref): "edit-\(ref.kindName)-\(ref.id)"
        case .session(let kind, let rootID): "session-\(kind == .feed ? "feed" : "sleep")-\(rootID)"
        }
    }
}

private extension EntryRef {
    var kindName: String {
        switch self {
        case .feed: "feed"
        case .sleep: "sleep"
        case .diaper: "diaper"
        case .supplement: "supplement"
        case .measurement: "measurement"
        case .immunization: "immunization"
        case .pump: "pump"
        }
    }
}

public extension LogSheet {
    /// A session's list of parts: a full-height system sheet, not a fitted
    /// one — it scrolls a varying number of rows. Everything else is a panel
    /// presented with `fittedSheet`.
    @MainActor
    func opensSessionList(store: BabyStore, babyID: UUID) -> Bool {
        switch self {
        case .session: return true
        case .editEntry(let ref):
            if case .session = EditDestination.resolve(ref, store: store, babyID: babyID) { return true }
            return false
        default: return false
        }
    }
}

/// The sheet an entry opens in edit mode, decided from the entry as it is
/// when the sheet opens.
enum EditDestination: Equatable {
    case diaper(UUID)
    case supplement(UUID)
    case solids(feedID: UUID)
    /// A feed of exactly one bottle.
    case bottle(feedID: UUID, componentID: UUID)
    /// A lone breast part or a sleep.
    case part(PartEditTarget)
    /// An older phone's combined feed, whose several parts only the session
    /// sheet can list.
    case session(SessionKind, rootID: UUID)
    case measurement(UUID)
    case immunization(UUID)
    case pump(UUID)
    /// Deleted since the tap — on this phone or the other — or never an
    /// entry of that kind.
    case gone

    @MainActor
    static func resolve(_ ref: EntryRef, store: BabyStore, babyID: UUID) -> EditDestination {
        if case .measurement(let id) = ref {
            let found = (try? store.measurements(for: babyID))?.contains { $0.id == id } ?? false
            return found ? .measurement(id) : .gone
        }
        // A pump belongs to the family: found by id alone, whichever baby's
        // screen asked.
        if case .pump(let id) = ref {
            return (try? store.pump(id: id)) != nil ? .pump(id) : .gone
        }
        guard let item = try? store.entry(id: ref.id, for: babyID) else { return .gone }
        switch (ref, item) {
        case (.diaper, .diaper(let diaper)):
            return .diaper(diaper.id)
        case (.supplement, .supplement(let med)):
            return .supplement(med.id)
        case (.immunization, .immunization(let shot)):
            return .immunization(shot.id)
        case (.sleep, .sleep(let sleep)):
            return .part(.sleep(sleepID: sleep.id))
        case (.feed, .feed(let feed)):
            // Solids open their own panel, even when added to a feed: the
            // panel edits their foods, which the session sheet only lists.
            if feed.kind == .solids { return .solids(feedID: feed.id) }
            guard feed.components.count == 1, let only = feed.components.first else {
                return .session(.feed, rootID: feed.effectiveFeedingSessionID)
            }
            switch only {
            case .breast: return .part(.breast(feedID: feed.id, componentID: only.id))
            case .bottle: return .bottle(feedID: feed.id, componentID: only.id)
            case .solids: return .solids(feedID: feed.id)
            }
        default:
            return .gone
        }
    }
}

public struct LogSheetView: View {
    let sheet: LogSheet
    let store: BabyStore
    let babyID: UUID
    let unit: VolumeUnit
    let homeOrder: [HomeActionKind]
    let onDone: () -> Void
    let onSwitchTo: (LogSheet) -> Void
    let onEditHomeButtons: (() -> Void)?

    public init(
        sheet: LogSheet,
        store: BabyStore,
        babyID: UUID,
        unit: VolumeUnit = .millilitres,
        homeOrder: [HomeActionKind] = HomeActionKind.defaultOrder,
        onDone: @escaping () -> Void,
        onSwitchTo: @escaping (LogSheet) -> Void,
        onEditHomeButtons: (() -> Void)? = nil
    ) {
        self.sheet = sheet
        self.store = store
        self.babyID = babyID
        self.unit = unit
        self.homeOrder = homeOrder
        self.onDone = onDone
        self.onSwitchTo = onSwitchTo
        self.onEditHomeButtons = onEditHomeButtons
    }

    public var body: some View {
        switch sheet {
        case .bottle:
            BottleSheet(store: store, babyID: babyID, unit: unit, mode: .start, onDone: onDone)
        case .bottleStopped(let timerID):
            BottleSheet(store: store, babyID: babyID, unit: unit, mode: .stopped(timerID: timerID), onDone: onDone)
        case .bottleAmount(let feedID):
            BottleSheet(store: store, babyID: babyID, unit: unit, mode: .amount(feedID: feedID), onDone: onDone)
        case .solids:
            SolidsSheet(store: store, babyID: babyID, onDone: onDone)
        // No detent here for any sheet: each sets its own, full height or one
        // fixed height — see `SheetSizing.swift`.
        case .measurement:
            MeasurementSheet(store: store, mode: .create(babyID: babyID), onDone: onDone)
        case .supplement:
            SupplementSheet(store: store, babyID: babyID, onDone: onDone)
        case .immunization:
            ImmunizationSheet(store: store, babyID: babyID, onDone: onDone)
        case .pump:
            PumpSheet(store: store, mode: .start, unit: unit, onDone: onDone, onSwitchTo: onSwitchTo)
        case .pumpAmount(let pumpID):
            PumpAmountSheet(store: store, pumpID: pumpID, unit: unit, onDone: onDone)
        case .diaper:
            DiaperSheet(store: store, babyID: babyID, onDone: onDone)
        case .meds:
            MedsSheet(store: store, babyID: babyID, onSwitchTo: onSwitchTo, onDone: onDone)
        case .more:
            MoreSheet(store: store, babyID: babyID, order: homeOrder, onSwitchTo: onSwitchTo,
                      onEditHomeButtons: onEditHomeButtons, onDone: onDone)
        case .pastEntry:
            PastEntrySheet(store: store, babyID: babyID, unit: unit, onDone: onDone)
        case .editEntry(let ref):
            EditSheetRouter(ref: ref, store: store, babyID: babyID, unit: unit, onDone: onDone)
        case .session(let kind, let rootID):
            SessionSheetHost(kind: kind, rootID: rootID, store: store, babyID: babyID, unit: unit, onDone: onDone)
        }
    }
}

/// Opens an entry's own sheet in edit mode. The destination and any model
/// are made once, when the sheet opens: re-deciding on every render would
/// re-read the store each time, and a sheet that changed kind under the
/// parent's finger would be worse than one that notices its entry is gone.
private struct EditSheetRouter: View {
    let store: BabyStore
    let babyID: UUID
    let unit: VolumeUnit
    let onDone: () -> Void
    @State private var destination: EditDestination
    @State private var partModel: PartEditModel?
    @State private var sessionModel: SessionSheetModel?
    @State private var measurement: MeasurementCheckIn?

    init(ref: EntryRef, store: BabyStore, babyID: UUID, unit: VolumeUnit, onDone: @escaping () -> Void) {
        self.store = store
        self.babyID = babyID
        self.unit = unit
        self.onDone = onDone
        let destination = EditDestination.resolve(ref, store: store, babyID: babyID)
        _destination = State(initialValue: destination)
        switch destination {
        case .part(let target):
            _partModel = State(initialValue: PartEditModel(store: store, babyID: babyID, target: target, unit: unit))
        case .session(let kind, let rootID):
            _sessionModel = State(initialValue: SessionSheetModel(store: store, babyID: babyID, kind: kind,
                                                                  rootID: rootID, unit: unit))
        case .measurement(let id):
            _measurement = State(initialValue: (try? store.measurements(for: babyID))?.first { $0.id == id })
        default:
            break
        }
    }

    var body: some View {
        switch destination {
        case .diaper(let id):
            DiaperSheet(store: store, babyID: babyID, editing: id, onDone: onDone)
        case .supplement(let id):
            SupplementSheet(store: store, babyID: babyID, editing: id, onDone: onDone)
        case .immunization(let id):
            ImmunizationSheet(store: store, babyID: babyID, editing: id, onDone: onDone)
        case .pump(let id):
            PumpSheet(store: store, mode: .edit(id), unit: unit, onDone: onDone)
        case .solids(let feedID):
            SolidsSheet(store: store, babyID: babyID, editing: feedID, onDone: onDone)
        case let .bottle(feedID, componentID):
            BottleSheet(store: store, babyID: babyID, unit: unit,
                        mode: .edit(feedID: feedID, componentID: componentID), onDone: onDone)
        case .part:
            if let partModel { PartEditSheet(model: partModel, onDone: onDone) }
        case .session:
            if let sessionModel { SessionSheet(model: sessionModel, unit: unit, onDone: onDone) }
        case .measurement:
            if let measurement {
                MeasurementSheet(store: store, mode: .edit(entry: measurement), onDone: onDone)
            } else {
                GoneSheet(onDone: onDone)
            }
        case .gone:
            GoneSheet(onDone: onDone)
        }
    }
}

/// The session sheet with its model made once, when the sheet opens.
private struct SessionSheetHost: View {
    let unit: VolumeUnit
    let onDone: () -> Void
    @State private var model: SessionSheetModel

    init(kind: SessionKind, rootID: UUID, store: BabyStore, babyID: UUID, unit: VolumeUnit,
         onDone: @escaping () -> Void) {
        self.unit = unit
        self.onDone = onDone
        _model = State(initialValue: SessionSheetModel(store: store, babyID: babyID, kind: kind,
                                                       rootID: rootID, unit: unit))
    }

    var body: some View {
        SessionSheet(model: model, unit: unit, onDone: onDone)
    }
}

/// The entry went between the tap and the sheet — deleted on the other
/// phone a moment earlier. There is nothing to edit, so the sheet closes,
/// the same as an open editor does when its entry goes.
private struct GoneSheet: View {
    let onDone: () -> Void

    var body: some View {
        Color.clear.onAppear(perform: onDone)
    }
}
