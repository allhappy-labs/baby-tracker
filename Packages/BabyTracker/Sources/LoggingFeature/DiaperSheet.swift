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
    public static let timeLabel = LoggingL10n.text("Time")
    public static let detailHeader = LoggingL10n.text("Details")
    public static let consistencyLabel = LoggingL10n.text("Consistency")
    public static let consistencyPrompt = LoggingL10n.text("Seedy, runny, firm")
    public static let colorLabel = LoggingL10n.text("Colour")
    public static let colorPrompt = LoggingL10n.text("Mustard, green, dark")
    public static let noteLabel = LoggingL10n.text("Note")
    public static let notePrompt = LoggingL10n.text("Optional")
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

/// Two taps: open, choose. Saves immediately — no confirmation screen.
/// `.dry` is deliberately absent from logging; only edit mode offers it.
///
/// Edit mode (`editing:` an entry's id) is the same sheet with the entry's
/// values: its time with the date at the top, the choices as a selection
/// rather than a save, Dry as a fourth choice so no recorded value is lost
/// (ruling H-R3), and Save and Delete at the bottom.
public struct DiaperSheet: View {
    let store: BabyStore
    let babyID: UUID
    let onDone: () -> Void

    /// Present in edit mode only. Create mode keeps its own `@State` below
    /// and never touches a model.
    @State private var editModel: DiaperEditModel?
    /// `onDone` runs once even when a delete is followed by the revision
    /// bump that marks the entry gone.
    @State private var isFinished = false

    @State private var when: Date
    /// Collapsed, and never on the critical path: the fast path is still
    /// open, choose — two taps, and the disclosure is below both of them.
    @State private var isShowingDetail = false
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
    /// How the choices are laid out. Only the width decides that, so the
    /// taller stated height it asks for cannot feed back into the choice.
    @State private var choiceLayout: ChoiceLayout = .row

    private enum ChoiceLayout { case row, grid, stacked }

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
    private func log(_ contents: DiaperContents) throws {
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
                .onAppear { choiceLayout = .stacked }
        } else {
            ViewThatFits(in: .horizontal) {
                EqualWidthRow(spacing: 10) {
                    ForEach(offered, id: \.self) { contents in
                        choice(contents, stacked: true)
                    }
                }
                .onAppear { choiceLayout = .row }
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
                    .onAppear { choiceLayout = .grid }
                }
                stackedChoices
                    .onAppear { choiceLayout = .stacked }
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

    /// What the choices add to the stated height over one row: a second row
    /// of two, or a stack of three or four.
    private var choiceLayoutExtra: CGFloat {
        switch choiceLayout {
        case .row: 0
        case .grid: 78
        case .stacked: offered.count == 4 ? 184 : 110
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
                try log(contents)
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
        // A stated sheet height that comes up short must not be absorbed by
        // squashing the three choices — they are the point of the sheet.
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityAddTraits(editModel != nil && isSelected ? .isSelected : [])
        .accessibilityIdentifier("diaper.\(contents.rawValue)")
    }

    /// The two optional fields as one grouped card, the way Settings and every
    /// iOS 26 form draws them: a label on the left, the field on the right, a
    /// hairline between. `.roundedBorder` drew them as black wells on a dark
    /// sheet — low contrast, and the look of iOS 6.
    private var detailFields: some View {
        VStack(spacing: 0) {
            detailRow(DiaperSheetCopy.consistencyLabel, text: field(\.consistency, else: $consistency), prompt: DiaperSheetCopy.consistencyPrompt, identifier: "diaper.consistency")
            Divider().padding(.leading, 16)
            detailRow(DiaperSheetCopy.colorLabel, text: field(\.color, else: $color), prompt: DiaperSheetCopy.colorPrompt, identifier: "diaper.colour")
            Divider().padding(.leading, 16)
            detailRow(DiaperSheetCopy.noteLabel, text: field(\.note, else: $note), prompt: DiaperSheetCopy.notePrompt, identifier: "diaper.note")
        }
        .background(.white.opacity(0.08), in: .rect(cornerRadius: 22, style: .continuous))
    }

    /// The edit model's field in edit mode, the sheet's own state otherwise.
    private func field(_ keyPath: ReferenceWritableKeyPath<DiaperEditModel, String>, else own: Binding<String>) -> Binding<String> {
        guard let editModel else { return own }
        return Binding(get: { editModel[keyPath: keyPath] }, set: { editModel[keyPath: keyPath] = $0 })
    }

    /// Label beside field at the ordinary sizes; label above field at the
    /// accessibility ones, where "Consistency" alone is wider than half the
    /// card and one of the two had to be cut.
    private func detailRow(_ title: String, text: Binding<String>, prompt: String, identifier: String) -> some View {
        let label = Text(title)
            .foregroundStyle(Palette.primaryText)
        let field = TextField(title, text: text, prompt: Text(prompt))
            .foregroundStyle(Palette.primaryText)
            // Outside a Form the field's title is not rendered at all, so
            // VoiceOver read the prompt's examples and never said what the
            // field was for.
            .accessibilityLabel(title)
            .accessibilityIdentifier(identifier)
        return Group {
            if typeSize.isAccessibilitySize {
                VStack(alignment: .leading, spacing: 4) {
                    label
                    field
                }
                .padding(.vertical, 10)
            } else {
                HStack(spacing: 12) {
                    // The label keeps its full width; the field takes what is
                    // left and scrolls its own text.
                    label.fixedSize().layoutPriority(1)
                    field.multilineTextAlignment(.trailing)
                }
            }
        }
        .padding(.horizontal, 16)
        .frame(minHeight: 48)
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
        NavigationStack {
            scrollingAtAccessibilitySizes(VStack(spacing: 14) {
                // Edit mode's title row: the entry's own time, with its date.
                if let editModel {
                    EditTimeRow(
                        date: Binding(get: { editModel.when }, set: { editModel.when = $0 }),
                        latest: editModel.latestAllowedDate,
                        identifier: "diaper.time",
                        stacksAtAccessibilitySizes: true
                    )
                }

                // One row where the words fit, not three stacked slabs.
                // Stacked full-width buttons at 64pt each, in a sheet sized for
                // a row, pushed the first one under the navigation bar, where
                // its fill drew over the title and the word "Diaper" rendered
                // on top of "Wet" — so a stack asks for a taller sheet below.
                //
                // Still tap-to-save rather than a select-then-confirm radio:
                // this sheet's whole point is two taps — open, choose — and a
                // radio would add a third at the moment a parent has one hand
                // free.
                choices
                    .padding(.top, 8)

                if editModel == nil {
                    DatePicker(DiaperSheetCopy.timeLabel, selection: $when, displayedComponents: [.hourAndMinute])
                        .padding(.top, 10)
                }

                DisclosureGroup(isExpanded: $isShowingDetail) {
                    detailFields
                        .padding(.top, 6)
                } label: {
                    // On the label, because that is the actionable row — a
                    // minHeight on the group itself would be measured across
                    // the fields once it is open.
                    Text(DiaperSheetCopy.detailHeader)
                        .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                        .accessibilityIdentifier("diaper.details")
                }

            }
            .padding(20))
            .frame(maxHeight: .infinity, alignment: .top)
            .editFooter(
                isPresented: editModel != nil,
                canSave: editModel?.canSave ?? false,
                identifierPrefix: "diaper",
                onSave: save,
                onDelete: delete
            )
            // Grows when the disclosure opens, because the measurement is of
            // what the content would like to be rather than of what the sheet
            // gave it. The `Spacer` that used to hold the bottom open is gone
            // with it — it was filling space this no longer allocates.
            // Two heights, because this sheet has two sizes: the disclosure
            // doubles as the difference between them. At `.medium` the
            // collapsed sheet was 40% empty and the expanded one still was.
            // Stacked, the three choices are about 110 pt taller than a row.
            // Edit mode moves the time row to the top and adds the footer.
            .sheetHeight((isShowingDetail ? 470 : 300) + choiceLayoutExtra + (editModel == nil ? 0 : EditFooter.height))
            .navigationTitle(DiaperSheetCopy.title)
            .editTitle(isPresented: editModel != nil, icon: .diaper, tint: Palette.diaper, label: DiaperSheetCopy.title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(DiaperSheetCopy.cancelLabel, action: onDone)
                }
            }
            .editLifecycle(isPresented: editModel != nil, revision: store.revision,
                           isGone: editModel?.isGone ?? false, reload: { editModel?.reload() }, onGone: finish)
        }
    }

    /// In edit mode at the accessibility sizes the sheet is full height
    /// and its content can still be taller than the screen — the time row,
    /// four stacked choices and Details over the footer. Without a scroll
    /// view the VStack overflowed upwards, and the time row ran under the
    /// navigation bar with its label cut to "D…" (History redesign, Task 7,
    /// `ax5-diaper-edit`). Create mode and the smaller sizes keep the
    /// plain, stated-height layout.
    @ViewBuilder
    private func scrollingAtAccessibilitySizes(_ content: some View) -> some View {
        if editModel != nil, typeSize.isAccessibilitySize {
            ScrollView { content }
        } else {
            content
        }
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
