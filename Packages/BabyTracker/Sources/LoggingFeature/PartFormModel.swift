import Foundation
import Observation
import BabyCore
import BabyStore

/// What the part panel's fields read and write, in new mode and edit mode
/// alike, so both draw the same rows.
@MainActor
public protocol PartFormModel: AnyObject, Observable {
    var kind: PartKind { get }
    var side: BreastSide { get set }
    var start: Date { get set }
    var end: Date? { get set }
    var volumeML: Double { get set }
    var contents: BottleContents { get set }
    var sleepKind: SleepKind? { get set }
    var note: String { get set }
    var unit: VolumeUnit { get }
    var chips: [Double] { get }
    func label(for ml: Double) -> String
    func step(_ direction: Int)
    var latestAllowedDate: Date { get }
    var validationMessage: String? { get }
    var canSave: Bool { get }
}

extension PartEditModel: PartFormModel {}

/// A breast side, bottle or sleep logged after the fact — the part panel in
/// new mode. Opens ten minutes ago to now, as Past entry did.
@MainActor @Observable
public final class NewPartModel: PartFormModel {
    private let store: BabyStore
    private let babyID: UUID
    public let unit: VolumeUnit

    /// Breast and sleep need an end; switching to one gives it back. A
    /// bottle logged after the fact is a moment unless the user adds an end
    /// time, so switching to it clears whatever end breast or sleep had.
    public var kind: PartKind {
        didSet {
            if kind == .bottle {
                end = nil
            } else if end == nil {
                end = min(store.now, start.addingTimeInterval(600))
            }
        }
    }
    public var side: BreastSide = .left
    public var start: Date
    public var end: Date?
    public var volumeML: Double
    public var contents: BottleContents
    public var sleepKind: SleepKind?
    public var note = ""

    public init(store: BabyStore, babyID: UUID, unit: VolumeUnit = .millilitres, kind: PartKind = .breast) {
        self.store = store
        self.babyID = babyID
        self.unit = unit
        self.kind = kind
        // `didSet` does not run here: a bottle opens as a moment, like a
        // switch to one.
        self.end = kind == .bottle ? nil : store.now
        self.start = store.now.addingTimeInterval(-600)
        let defaults = LogDefaults(store: store)
        self.volumeML = (try? defaults.lastBottleVolumeML(for: babyID)) ?? LogDefaults.fallbackVolumeML
        self.contents = (try? defaults.lastBottleContents(for: babyID)) ?? LogDefaults.fallbackContents
    }

    public var latestAllowedDate: Date { store.now }

    public var validationMessage: String? {
        TimeSpan(start: start, end: end)
            .problem(latest: store.now, minimumSeconds: kind == .breast ? 30 : 1)
            .map(TimeSpanCopy.message)
    }

    public var canSave: Bool {
        validationMessage == nil && (kind == .bottle ? volumeML > 0 : end != nil)
    }

    public var chips: [Double] { BottleSheetModel.chips(for: unit) }
    public func label(for ml: Double) -> String { BottleSheetModel.label(for: ml, unit: unit) }
    public func step(_ direction: Int) {
        volumeML = max(0, volumeML + Double(direction) * BottleSheetModel.stepAmount(for: unit))
    }

    public func save() throws {
        let note = EditModel.trimmedToNil(note)
        switch kind {
        case .breast:
            guard let end else { throw EditModeError.invalid }
            _ = try store.logBreast(side: side, from: start, to: end, for: babyID, note: note)
        case .bottle:
            _ = try store.logBottle(volumeML: volumeML, contents: contents, for: babyID, at: start,
                                    endedAt: end, note: note)
        case .sleep:
            guard let end else { throw EditModeError.invalid }
            _ = try store.logSleep(from: start, to: end, for: babyID, kindOverride: sleepKind, note: note)
        }
    }
}
