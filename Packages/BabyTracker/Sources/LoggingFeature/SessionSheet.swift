import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// A feed or sleep session in one sheet (mockup G2): its time and totals
/// with Done, the Join row, then each part with the gap before it in grey on
/// the left and **Split here** on the right, Add part, and Delete feed /
/// Delete sleep. Tapping a part slides its editor in, inside the sheet's own
/// navigation stack; Save or Delete there returns to the session.
///
/// A view-only caregiver (ruling H-R6) sees the same session with nothing to
/// tap: no Join, Split, Add, Delete, swipe or editor.
public struct SessionSheet: View {
    @State private var model: SessionSheetModel
    /// The part being edited, in its own sheet over this one. Pushed inside
    /// this sheet instead, the editor kept the session's taller layout until
    /// the push finished, so its Save and Delete popped in late and the sheet
    /// jumped.
    @State private var editing: PartEditTarget?
    /// Solids being added from Add part, in the Solids panel over this sheet.
    @State private var addingSolids: SolidsLogModel?
    @State private var isConfirmingDelete = false
    /// `onDone` runs once even when a delete is followed by the revision
    /// bump that marks the session gone.
    @State private var isFinished = false
    private let unit: VolumeUnit
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(model: SessionSheetModel, unit: VolumeUnit, onDone: @escaping () -> Void) {
        _model = State(initialValue: model)
        self.unit = unit
        self.onDone = onDone
    }

    public var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                header
                List {
                    if model.canEdit, model.join.previous != nil {
                        Section { JoinRow(model: model.join) }
                    }

                    Section {
                        ForEach(Array(model.parts.enumerated()), id: \.element.id) { index, part in
                            if showsGapRow(part) {
                                gapRow(part, index: index)
                            }
                            partRow(part, index: index)
                        }
                    }

                    if model.canEdit {
                        Section { addPart }
                    }
                }
                // Full height: a session holds a varying number of parts, and a
                // sheet resized to each of them jumped.
                .fullHeightSheet()
                // The list's own bottom margin would stack on the footer's padding.
                .contentMargins(.bottom, 8, for: .scrollContent)
                // Gap rows are a line of small text, not a full row.
                .environment(\.defaultMinListRowHeight, 24)
                #if os(iOS)
                .listSectionSpacing(.compact)
                // The header is drawn below, with Done beside the time, as in the
                // mockup; a bar above it would only leave an empty band.
                .toolbar(.hidden, for: .navigationBar)
                #endif
                .navigationTitle(model.title)
                // The kit's footer, pinned under the list: Delete as text on its own row.
                if model.canEdit {
                    SheetFooter(destructive: SheetAction(
                        model.kind == .feed ? SessionSheetCopy.deleteFeed : SessionSheetCopy.deleteSleep,
                        identifier: "sessionSheet.delete") { isConfirmingDelete = true })
                }
            }
            .fittedSheet(item: $editing) { target in
                Group {
                    if case let .solids(feedID, _) = target {
                        SolidsSheet(store: model.store, babyID: model.babyID, editing: feedID) {
                            if editing == target { editing = nil }
                        }
                    } else {
                        PartEditSheet(model: model.editorModel(for: target), showsJoinRow: false) {
                            if editing == target { editing = nil }
                        }
                    }
                }
                // A fitted sheet does not inherit custom environment values.
                .environment(\.errorReporter, errorReporter)
            }
            .fittedSheet(item: $addingSolids) { solids in
                SolidsSheet(store: model.store, babyID: model.babyID, adding: solids) {
                    if addingSolids === solids { addingSolids = nil }
                }
                .environment(\.errorReporter, errorReporter)
            }
            .confirmDelete(isPresented: $isConfirmingDelete, title: deleteConfirmTitle,
                           deleteTitle: EditChromeCopy.delete, keepTitle: EditChromeCopy.keep,
                           identifierPrefix: "sessionSheet", onDelete: deleteSession)
        }
        // Split here and the other system controls in the session's colour, not the system blue.
        .tint(tint)
        .editLifecycle(revision: model.store.revision, isGone: model.isGone, reload: model.reload, onGone: finish)
    }

    // MARK: - Header

    private var header: some View {
        HStack(alignment: .top, spacing: Spacing.m) {
            // The ✕ keeps the old text title ("Done") as its label.
            SheetCloseButton(title: SessionSheetCopy.done, identifier: "sessionSheet.done", action: finish)
            VStack(alignment: .leading, spacing: 2) {
                Text(model.title)
                    .textRole(.sheetTitle)
                    .monospacedDigit()
                    .accessibilityAddTraits(.isHeader)
                    .accessibilityIdentifier("sessionSheet.title")
                if !model.totals.isEmpty {
                    Text(model.totals)
                        .font(.subheadline)
                        .monospacedDigit()
                        .foregroundStyle(Palette.secondaryText)
                        .accessibilityIdentifier("sessionSheet.totals")
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(.horizontal, Spacing.l)
        .padding(.top, Spacing.l)
        .padding(.bottom, Spacing.xs)
    }

    // MARK: - Parts

    private func partRow(_ part: SessionPart, index: Int) -> some View {
        let label = Text(part.label).foregroundStyle(Palette.primaryText)
        let detail = Text(part.detail)
            .font(.subheadline)
            .monospacedDigit()
            .foregroundStyle(Palette.secondaryText)
            .fixedSize(horizontal: true, vertical: false)
        let content = Group {
            if typeSize.isAccessibilitySize {
                // The time range stays on one line; it goes under the label.
                HStack(alignment: .top, spacing: 12) {
                    BabyIconView(part.icon, size: 22).foregroundStyle(tint)
                    VStack(alignment: .leading, spacing: Spacing.xs) { label; detail }
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            } else {
                HStack(spacing: 12) {
                    BabyIconView(part.icon, size: 22).foregroundStyle(tint)
                    label.frame(maxWidth: .infinity, alignment: .leading)
                    detail
                }
            }
        }
        .frame(minHeight: 44)
        .accessibilityElement(children: .combine)

        return Group {
            if model.canEdit {
                Button {
                    editing = part.target
                } label: {
                    HStack(spacing: 8) {
                        content
                        Image(systemName: "chevron.right")
                            .font(.footnote.weight(.semibold))
                            .foregroundStyle(Palette.secondaryText)
                            .accessibilityHidden(true)
                    }
                    .contentShape(.rect)
                }
                .buttonStyle(.plain)
                    .swipeActions(edge: .trailing, allowsFullSwipe: true) {
                        Button(role: .destructive) {
                            deletePart(part)
                        } label: {
                            Label(EditChromeCopy.delete, systemImage: "trash")
                        }
                        .accessibilityIdentifier("sessionSheet.part.\(index).delete")
                    }
            } else {
                content
            }
        }
        .listRowSeparator(.hidden)
        .listRowInsets(Self.partInsets)
        .accessibilityIdentifier("sessionSheet.part.\(index)")
    }

    /// Part rows sit close; the gap line between two of them is a line of
    /// small text, not another full row (mockup G2).
    private static let partInsets = EdgeInsets(top: 2, leading: Spacing.xl, bottom: 2, trailing: Spacing.xl)
    private static let gapInsets = EdgeInsets(top: 0, leading: Spacing.xl, bottom: 0, trailing: Spacing.xl)

    /// The line between two parts: the gap in small grey text on the left
    /// and Split here on the right, only where Split can cut (ruling H-R2).
    /// No scissors, no dashed line. The row has no insets of its own, so it
    /// is as tall as Split here's 44-point tap target and no taller: a list
    /// row's touches end at its edge, so the target cannot borrow height
    /// from the parts around it. With no Split, the line is shorter still.
    private func gapRow(_ part: SessionPart, index: Int) -> some View {
        let canSplit = model.canEdit && part.canSplitBefore
        return HStack(spacing: 12) {
            Text(part.gapBefore ?? "")
                .font(.caption)
                .monospacedDigit()
                .foregroundStyle(Palette.secondaryText)
                .frame(maxWidth: .infinity, alignment: .leading)
                .accessibilityIdentifier("sessionSheet.gap.\(index)")
            if canSplit {
                Button { split(before: part) } label: {
                    Text(SessionSheetCopy.splitHere)
                        .font(.caption.weight(.semibold))
                        // Wider and taller to the touch than it looks.
                        .padding(.leading, 12)
                        .frame(minWidth: 44, minHeight: 44, alignment: .trailing)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.borderless)
                .accessibilityLabel(SessionSheetCopy.splitBefore(part.label))
                .accessibilityIdentifier("sessionSheet.split.\(index)")
            }
        }
        .frame(minHeight: canSplit ? 44 : 24)
        .listRowSeparator(.hidden)
        .listRowInsets(Self.gapInsets)
    }

    private func showsGapRow(_ part: SessionPart) -> Bool {
        part.gapBefore != nil || (model.canEdit && part.canSplitBefore)
    }

    // MARK: - Add and Delete

    /// Left, Right, Bottle and Solids for a feed; a sleep has the one kind, so its
    /// Add part adds a sleep at once rather than opening a one-item menu.
    /// A kind that would end in the future is off (ruling H-R18) until the
    /// session ended long enough ago; while one is off, the row re-checks
    /// every few seconds, as nothing else redraws the sheet as time passes.
    @ViewBuilder
    private var addPart: some View {
        if AddPartKind.offered(for: model.kind).allSatisfy(model.canAdd) {
            addPartControl
        } else {
            TimelineView(.periodic(from: .now, by: 5)) { _ in addPartControl }
        }
    }

    @ViewBuilder
    private var addPartControl: some View {
        let offered = AddPartKind.offered(for: model.kind)
        if offered.count == 1, let only = offered.first {
            Button { add(only) } label: { addPartLabel(isEnabled: model.canAdd(only)) }
                .disabled(!model.canAdd(only))
                .accessibilityIdentifier("sessionSheet.addPart")
        } else {
            Menu {
                ForEach(offered, id: \.self) { kind in
                    Button(kind.title) { add(kind) }
                        .disabled(!model.canAdd(kind))
                        .accessibilityIdentifier("sessionSheet.add.\(kind)")
                }
            } label: {
                addPartLabel(isEnabled: true)
            }
            .accessibilityIdentifier("sessionSheet.addPart")
        }
    }

    /// Dimmed by hand while off: the tint is set explicitly, so the
    /// button's own disabled look does not reach it (it looked as on as
    /// ever in the screenshots).
    private func addPartLabel(isEnabled: Bool) -> some View {
        Label(SessionSheetCopy.addPart, systemImage: "plus")
            .foregroundStyle(tint)
            .opacity(PrimaryButtonStyle.opacity(isEnabled: isEnabled))
            .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
            .contentShape(.rect)
    }

    private var deleteConfirmTitle: String {
        model.kind == .feed ? SessionSheetCopy.deleteFeedConfirmTitle : SessionSheetCopy.deleteSleepConfirmTitle
    }

    // MARK: - Layout

    private var tint: Color { model.kind == .feed ? Palette.feed : Palette.sleep }

    // MARK: - Actions

    private var subject: String { model.kind == .feed ? ErrorSubject.feed : ErrorSubject.sleep }

    private func split(before part: SessionPart) {
        _ = performEdit({ try model.split(before: part.id) }, failure: ErrorBannerCopy.saveFailed(what: subject),
                        reporter: errorReporter)
    }

    private func add(_ kind: AddPartKind) {
        // Solids are their foods: the panel opens first, and they are
        // written once named.
        if kind == .solids {
            addingSolids = model.newSolidsModel()
            return
        }
        var added: PartEditTarget?
        if performEdit({ added = try model.addPart(kind) }, failure: ErrorBannerCopy.saveFailed(what: subject),
                       reporter: errorReporter), let added {
            editing = added
        }
    }

    private func deletePart(_ part: SessionPart) {
        _ = performEdit({ try model.deletePart(part.id) }, failure: ErrorBannerCopy.deleteFailed(what: subject),
                        reporter: errorReporter)
    }

    private func deleteSession() {
        if performEdit(model.deleteSession, failure: ErrorBannerCopy.deleteFailed(what: subject),
                       reporter: errorReporter) {
            finish()
        }
    }

    private func finish() {
        guard !isFinished else { return }
        isFinished = true
        onDone()
    }
}
