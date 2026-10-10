import SwiftUI
import Foundation
import BabyCore
import BabyStore
import BabyUI

/// Every string this sheet shows. Strings never live in a view: one baked
/// into a `View` cannot be read by a test.
public enum DiaperSheetCopy {
    public static let title = LoggingL10n.text("Diaper")
    public static let cancelLabel = LoggingL10n.text("Cancel")
    public static let consistencyLabel = LoggingL10n.text("Consistency")
    public static let consistencyPrompt = LoggingL10n.text("Seedy, runny, firm")
    public static let colorLabel = LoggingL10n.text("Colour")
    public static let colorPrompt = LoggingL10n.text("Mustard, green, dark")
}

/// Edit mode's state and writes: the diaper's fields as a draft, saved in one
/// update or deleted as a tombstone.
///
/// Holds the entry's id, not the entry. The other phone may change or delete
/// it while the sheet is open, so every write fetches it afresh, and
/// `reload()` — which the sheet calls on every `store.revision` — notices
/// when it has gone.
@MainActor @Observable
public final class DiaperEditModel {
    private let store: BabyStore
    private let babyID: UUID
    public let diaperID: UUID

    public var contents: DiaperContents
    public var when: Date
    public var consistency: String
    public var color: String
    public var note: String
    /// True once the entry has vanished, for example after the other phone
    /// deleted it. The sheet closes on it.
    public private(set) var isGone: Bool

    /// The entry's fields as last read, so `reload()` can tell an untouched
    /// field (take the other phone's value) from an edited one (keep it).
    private var loaded: Draft

    private struct Draft: Equatable {
        var contents: DiaperContents
        var when: Date
        var consistency: String
        var color: String
        var note: String

        init(contents: DiaperContents, when: Date, consistency: String, color: String, note: String) {
            self.contents = contents
            self.when = when
            self.consistency = consistency
            self.color = color
            self.note = note
        }

        init(_ entry: DiaperEntry) {
            self.init(contents: entry.contents, when: entry.startedAt, consistency: entry.consistency ?? "",
                      color: entry.color ?? "", note: entry.note ?? "")
        }
    }

    public init(store: BabyStore, babyID: UUID, diaperID: UUID) {
        self.store = store
        self.babyID = babyID
        self.diaperID = diaperID
        let entry = Self.fetch(diaperID, babyID: babyID, in: store)
        let draft = entry.map(Draft.init) ?? Draft(contents: .wet, when: store.now, consistency: "", color: "", note: "")
        loaded = draft
        contents = draft.contents
        when = draft.when
        consistency = draft.consistency
        color = draft.color
        note = draft.note
        isGone = entry == nil
    }

    /// Now, or the entry's own time if it is somehow already ahead of the
    /// clock, so opening such an entry to fix its note stays saveable.
    public var latestAllowedDate: Date { max(store.now, loaded.when) }

    /// Opens Details when there is something in it to see.
    public var hasDetail: Bool { !consistency.isEmpty || !color.isEmpty || !note.isEmpty }

    public var canSave: Bool { !isGone && when <= latestAllowedDate }

    /// One `.update`. A refused write puts the entry back as it was, so the
    /// edit never sits unsaved in the context for an unrelated save to
    /// commit without its change-log entry.
    public func save() throws {
        guard let entry = Self.fetch(diaperID, babyID: babyID, in: store) else {
            isGone = true
            throw EditModeError.gone
        }
        guard canSave else { throw EditModeError.invalid }
        // The stored values exactly as they are, not as a draft would write
        // them: legacy or remote text with surrounding spaces must come back
        // untrimmed, or the restore itself is an unlogged change.
        let before = (entry.contents, entry.startedAt, entry.consistency, entry.color, entry.note, entry.lastModifiedAt)
        entry.contents = contents
        entry.startedAt = when
        entry.consistency = EditModel.trimmedToNil(consistency)
        entry.color = EditModel.trimmedToNil(color)
        entry.note = EditModel.trimmedToNil(note)
        do {
            try store.update(entry)
        } catch {
            (entry.contents, entry.startedAt, entry.consistency, entry.color, entry.note, entry.lastModifiedAt) = before
            throw error
        }
        loaded = Draft(entry)
    }

    /// A tombstone, never a row removal. Diapers are not in sessions, so the
    /// plain `softDelete` is the right call (ruling H-R11).
    public func delete() throws {
        guard let entry = Self.fetch(diaperID, babyID: babyID, in: store) else {
            isGone = true
            throw EditModeError.gone
        }
        let modifiedAt = entry.lastModifiedAt
        do {
            try store.softDelete(entry)
        } catch {
            entry.isTombstoned = false
            entry.lastModifiedAt = modifiedAt
            throw error
        }
    }

    /// Re-reads the entry after any write, this phone's or the other's.
    public func reload() {
        guard let entry = Self.fetch(diaperID, babyID: babyID, in: store) else {
            isGone = true
            return
        }
        let fresh = Draft(entry)
        EditModel.merge(&contents, loaded: loaded.contents, fresh: fresh.contents)
        EditModel.merge(&when, loaded: loaded.when, fresh: fresh.when)
        EditModel.merge(&consistency, loaded: loaded.consistency, fresh: fresh.consistency)
        EditModel.merge(&color, loaded: loaded.color, fresh: fresh.color)
        EditModel.merge(&note, loaded: loaded.note, fresh: fresh.note)
        loaded = fresh
    }

    /// Only from the active realm: after a switch to or from a family, an
    /// entry of the other realm is gone as far as this sheet is concerned.
    private static func fetch(_ id: UUID, babyID: UUID, in store: BabyStore) -> DiaperEntry? {
        guard case .diaper(let entry)? = try? store.entry(id: id, for: babyID),
              entry.realmKey == store.activeRealm.encodedKey else { return nil }
        return entry
    }
}

/// Two taps: open, choose. Saves immediately, at the Time row's date and
/// time — no confirmation screen. `.dry` is deliberately absent from
/// logging; only edit mode offers it.
///
/// Edit mode (`editing:` an entry's id) is the same panel with the entry's
/// values: the choices as a selection rather than a save, Dry as a fourth
/// choice so no recorded value is lost (ruling H-R3), and Save and Delete
/// at the bottom.
public struct DiaperSheet: View {
    let store: BabyStore
    let babyID: UUID
    let onDone: () -> Void

    /// Present in edit mode only. New mode keeps its own `@State` below.
    @State private var editModel: DiaperEditModel?
    /// `onDone` runs once even when a delete is followed by the revision
    /// bump that marks the entry gone.
    @State private var isFinished = false

    @State private var when: Date
    /// Edit mode's Details: open when the entry already has one.
    @State private var isShowingDetail = false
    /// New mode's details, written with the choice that saves.
    @State private var consistency = ""
    @State private var color = ""
    @State private var note = ""
    @Environment(\.errorReporter) private var errorReporter
    /// One row of three never fits at the accessibility sizes, and at the
    /// larger ordinary ones only when the words are short. Squeezing the
    /// words with `minimumScaleFactor` was the first attempt and the audit
    /// correctly called it clipping: text that has to shrink to fit has not
    /// scaled, it has been overruled.
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(store: BabyStore, babyID: UUID, editing diaperID: UUID? = nil, onDone: @escaping () -> Void) {
        self.store = store
        self.babyID = babyID
        self.onDone = onDone
        _when = State(wrappedValue: store.now)
        let model = diaperID.map { DiaperEditModel(store: store, babyID: babyID, diaperID: $0) }
        _editModel = State(wrappedValue: model)
        _isShowingDetail = State(wrappedValue: Self.shouldOpenDetailForScreenshot || model?.hasDetail == true)
    }

    /// What the sheet offers. Logging keeps the three-kind fast path; edit
    /// mode adds Dry, because an entry recorded as dry must be able to stay
    /// dry, and a wrong one must be correctable to it.
    public static func choices(editing: Bool) -> [DiaperContents] {
        editing ? DiaperContents.allCases : DiaperContents.fastPath
    }

    private var offered: [DiaperContents] { Self.choices(editing: editModel != nil) }

    /// Verification-only: lets a screenshot show the accordion already open
    /// without a tap. Every `-uiTest*` hook stays behind `#if DEBUG` — see
    /// `UITestSupport`'s own doc comment — so a release build always starts
    /// with Details collapsed, full stop, with no argument able to change it.
    #if DEBUG
    private static var shouldOpenDetailForScreenshot: Bool {
        ProcessInfo.processInfo.arguments.contains("-uiTestDiaperDetailsOpen")
    }
    #else
    private static var shouldOpenDetailForScreenshot: Bool { false }
    #endif

    /// Exactly one write, detail or no detail. It used to be two — log, then
    /// update with the detail — and a failure on the second left an entry
    /// saved behind a banner that said the save had failed, so tapping the
    /// same button again logged a SECOND diaper. The fast path's change log is
    /// unchanged: one `.create`, with or without the disclosure open.
    static func log(_ contents: DiaperContents, at when: Date, store: BabyStore, babyID: UUID,
                    consistency: String = "", color: String = "", note: String = "") throws {
        _ = try store.logDiaper(
            contents,
            for: babyID,
            at: when,
            consistency: EditModel.trimmedToNil(consistency),
            color: EditModel.trimmedToNil(color),
            note: EditModel.trimmedToNil(note)
        )
    }

    /// The three fast-path kinds as buttons that save on tap.
    ///
    /// Side by side when every word fits its third whole, stacked otherwise —
    /// always at the accessibility sizes, and at the larger ordinary ones
    /// when the words are long (Ukrainian at XXXL broke "Брудний" and
    /// "Змішане" mid-word across two lines). Shrinking the word to fit is the
    /// thing the audit calls clipping, so the row gives way instead.
    ///
    /// Still tap-to-save rather than a select-then-confirm radio: this sheet's
    /// whole point is two taps — open, choose — and a radio would add a third
    /// at the moment a parent has one hand free.
    ///
    /// Edit mode's four choices try a row, then two rows of two, then the
    /// stack: four in a row only fits the shortest words.
    @ViewBuilder
    private var choices: some View {
        if typeSize.isAccessibilitySize {
            stackedChoices
        } else {
            ViewThatFits(in: .horizontal) {
                EqualWidthRow(spacing: 10) {
                    ForEach(offered, id: \.self) { contents in
                        choice(contents, stacked: true)
                    }
                }
                if offered.count == 4 {
                    VStack(spacing: 10) {
                        EqualWidthRow(spacing: 10) {
                            ForEach(offered.prefix(2), id: \.self) { contents in
                                choice(contents, stacked: true)
                            }
                        }
                        EqualWidthRow(spacing: 10) {
                            ForEach(offered.suffix(2), id: \.self) { contents in
                                choice(contents, stacked: true)
                            }
                        }
                    }
                }
                stackedChoices
            }
        }
    }

    private var stackedChoices: some View {
        VStack(spacing: 10) {
            ForEach(offered, id: \.self) { contents in
                choice(contents, stacked: false)
            }
        }
    }

    /// `stacked` is about the glyph and the word, not about the row: side by
    /// side they need the icon above the label to stay narrow, stacked they
    /// have the whole width and read better as icon-then-label.
    ///
    /// In edit mode a tap selects, and Save writes: the entry already
    /// exists, and a stray tap must not rewrite it.
    private func choice(_ contents: DiaperContents, stacked: Bool) -> some View {
        let isSelected = editModel.map { $0.contents == contents } ?? true
        return Button {
            if let editModel {
                editModel.contents = contents
                return
            }
            do {
                try Self.log(contents, at: when, store: store, babyID: babyID,
                             consistency: consistency, color: color, note: note)
                errorReporter.clear()
                onDone()
            } catch {
                // The sheet deliberately stays open: dismissing on a failed
                // save is what made this invisible before.
                errorReporter.report(
                    ErrorBannerCopy.saveFailed(what: ErrorSubject.diaperChange),
                    error: error,
                    category: AppLog.store
                )
            }
        } label: {
            let glyph = Self.glyph(for: contents)
            let title = Text(contents.displayName).font(.subheadline.weight(.semibold))
            Group {
                if stacked {
                    VStack(spacing: 6) { glyph; title }
                } else {
                    HStack(spacing: 10) { glyph; title; Spacer(minLength: 0) }
                }
            }
            .frame(maxWidth: .infinity, minHeight: 64)
        }
        // Unselected choices in edit mode go grey, so the selected one is
        // told apart by colour as well as by its stronger stroke.
        .buttonStyle(.tracker(isSelected ? Palette.diaper : Palette.secondaryText,
                              emphasis: isSelected ? .primary : .secondary))
        // A sheet that comes up short scrolls rather than squashing the
        // choices — they are the point of the sheet.
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityAddTraits(editModel != nil && isSelected ? .isSelected : [])
        .accessibilityIdentifier("diaper.\(contents.rawValue)")
    }

    /// The icon set's contents glyph for wet, dirty and mixed. Dry has no
    /// contents to draw, so it is the empty diaper itself.
    @ViewBuilder
    private static func glyph(for contents: DiaperContents) -> some View {
        switch contents {
        case .wet: BabyIconView.diaperContents(.wet)
        case .dirty: BabyIconView.diaperContents(.dirty)
        case .mixed: BabyIconView.diaperContents(.mixed)
        case .dry: BabyIconView(.diaper, size: 20).foregroundStyle(Palette.diaper)
        }
    }

    public var body: some View {
            // The same panel as every other entry. New mode has no footer: a
            // choice saves, with the Time row's value. Edit mode's choices
            // select, and Save writes.
            EntryPanel(title: DiaperSheetCopy.title, icon: .diaper, tint: Palette.diaper,
                       canSave: editModel?.canSave ?? false, identifierPrefix: "diaper",
                       onCancel: onDone, onSave: saveAction, onDelete: deleteAction) {
                // One row where the words fit, not three stacked slabs. Still
                // tap-to-save rather than select-then-confirm: two taps — open,
                // choose — while a parent has one hand free.
                choices

                // The same dated row logging and editing: a change remembered
                // the morning after belongs to the day before.
                PanelSection {
                    EditTimeRow(date: timeBinding, latest: editModel?.latestAllowedDate ?? store.now,
                                identifier: "diaper.time")
                }

                // Opened, the sheet grows with it; the choice still saves
                // what is typed here.
                DetailsSection(isExpanded: $isShowingDetail, identifier: "diaper.details") {
                    DetailFieldRow(DiaperSheetCopy.consistencyLabel, text: field(\.consistency, else: $consistency),
                                   prompt: DiaperSheetCopy.consistencyPrompt, identifier: "diaper.consistency")
                    DetailFieldRow(DiaperSheetCopy.colorLabel, text: field(\.color, else: $color),
                                   prompt: DiaperSheetCopy.colorPrompt, identifier: "diaper.colour")
                    DetailFieldRow(EditChromeCopy.note, text: field(\.note, else: $note), identifier: "diaper.note")
                }
            }
            .editLifecycle(isPresented: editModel != nil, revision: store.revision,
                           isGone: editModel?.isGone ?? false, reload: { editModel?.reload() }, onGone: finish)
    }

    /// The edit model's field in edit mode, the sheet's own state otherwise.
    private func field(_ keyPath: ReferenceWritableKeyPath<DiaperEditModel, String>, else own: Binding<String>) -> Binding<String> {
        guard let editModel else { return own }
        return Binding(get: { editModel[keyPath: keyPath] }, set: { editModel[keyPath: keyPath] = $0 })
    }

    /// Edit mode only: in new mode a choice saves, and nothing deletes.
    private var saveAction: (() -> Void)? {
        guard editModel != nil else { return nil }
        return { save() }
    }

    private var deleteAction: (() -> Void)? {
        guard editModel != nil else { return nil }
        return { delete() }
    }

    /// The edit model's time in edit mode, the sheet's own otherwise.
    private var timeBinding: Binding<Date> {
        if let editModel { Bindable(editModel).when } else { $when }
    }

    // MARK: - Edit mode

    private func save() {
        guard let editModel else { return }
        if performEdit(editModel.save, failure: ErrorBannerCopy.saveFailed(what: ErrorSubject.diaperChange),
                       reporter: errorReporter) { finish() }
    }

    private func delete() {
        guard let editModel else { return }
        if performEdit(editModel.delete, failure: ErrorBannerCopy.deleteFailed(what: ErrorSubject.diaperChange),
                       reporter: errorReporter) { finish() }
    }

    private func finish() {
        guard !isFinished else { return }
        isFinished = true
        onDone()
    }
}

/// One row of equal widths. Its ideal width is the widest subview's times the
/// count, so `ViewThatFits` takes the row only when every subview fits its
/// share at its ideal width — an `HStack`'s ideal width is the sum, which
/// fits while the longest word still breaks inside an equal third.
private struct EqualWidthRow: Layout {
    let spacing: CGFloat

    private func gaps(_ subviews: Subviews) -> CGFloat {
        spacing * CGFloat(max(subviews.count - 1, 0))
    }

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        guard !subviews.isEmpty else { return .zero }
        let count = CGFloat(subviews.count)
        let width = proposal.width
            ?? (subviews.map { $0.sizeThatFits(.unspecified).width }.max() ?? 0) * count + gaps(subviews)
        let column = max((width - gaps(subviews)) / count, 0)
        let height = subviews.map { $0.sizeThatFits(ProposedViewSize(width: column, height: nil)).height }.max() ?? 0
        return CGSize(width: width, height: height)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        guard !subviews.isEmpty else { return }
        let column = max((bounds.width - gaps(subviews)) / CGFloat(subviews.count), 0)
        for (index, subview) in subviews.enumerated() {
            subview.place(
                at: CGPoint(x: bounds.minX + CGFloat(index) * (column + spacing), y: bounds.minY),
                anchor: .topLeading,
                proposal: ProposedViewSize(width: column, height: bounds.height)
            )
        }
    }
}
