import SwiftUI
import Observation
import BabyCore
import BabyStore
import BabyUI

/// One part's editor, the part panel in edit mode: a breast side's side; a
/// bottle's amount chips and contents; or a sleep's Nap / Night / Auto — then
/// Start and End, the note under Details, and Save and Delete at the bottom.
///
/// It always brings its own navigation bar and Cancel. Opened on its own from
/// History it shows the Join row (`showsJoinRow`, the default); opened over
/// the session sheet, which has the Join row already, it does not, and
/// `onDone` returns to the session.
public struct PartEditSheet: View {
    @State private var model: PartEditModel
    @State private var join: JoinRowModel?
    @State private var isShowingDetails: Bool
    /// `onDone` runs once even when a delete is followed by the revision
    /// bump that marks the part gone.
    @State private var isFinished = false
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    public init(model: PartEditModel, showsJoinRow: Bool = true, onDone: @escaping () -> Void) {
        _model = State(initialValue: model)
        _join = State(initialValue: showsJoinRow
            ? JoinRowModel(store: model.store, babyID: model.babyID, kind: model.target.sessionKind,
                           partOf: model.target.entryID)
            : nil)
        _isShowingDetails = State(initialValue: !model.note.isEmpty)
        self.onDone = onDone
    }

    public var body: some View {
        EntryPanel(title: title, icon: icon, tint: tint, canSave: model.canSave, identifierPrefix: "partEdit",
                   onCancel: onDone, onSave: save, onDelete: delete) {
            if let join, join.previous != nil {
                PanelSection { JoinRow(model: join) }
            }
            PartFields(model: model, isShowingDetails: $isShowingDetails)
        }
        .editLifecycle(revision: model.store.revision, isGone: model.isGone,
                       reload: { model.reload(); join?.reload() }, onGone: finish)
    }

    private var title: String {
        switch model.target {
        case .breast: PartEditCopy.breastTitle
        case .bottle: PartEditCopy.bottleTitle
        case .sleep: Tracker.sleep.title
        }
    }

    private var icon: BabyIcon {
        switch model.target {
        case .breast: model.side == .left ? .breastLeft : .breastRight
        case .bottle: .bottle
        case .sleep: .sleep
        }
    }

    private var tint: Color { model.target.sessionKind == .feed ? Palette.feed : Palette.sleep }

    // MARK: - Actions

    private func save() {
        var saved = false
        let subject = model.target.sessionKind == .feed ? ErrorSubject.feed : ErrorSubject.sleep
        // A refused draft keeps the sheet open with its message in the form.
        if performEdit({ saved = try model.save() }, failure: ErrorBannerCopy.saveFailed(what: subject),
                       reporter: errorReporter), saved || model.isGone {
            finish()
        }
    }

    private func delete() {
        let subject = model.target.sessionKind == .feed ? ErrorSubject.feed : ErrorSubject.sleep
        if performEdit(model.delete, failure: ErrorBannerCopy.deleteFailed(what: subject), reporter: errorReporter) {
            finish()
        }
    }

    private func finish() {
        guard !isFinished else { return }
        isFinished = true
        onDone()
    }
}

/// Something that already happened — a side, a bottle or a sleep — in the
/// same panel History edits it in, with a kind picker on top and Save only.
public struct NewPartSheet: View {
    @State private var model: NewPartModel
    @State private var isShowingDetails = false
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    public init(store: BabyStore, babyID: UUID, unit: VolumeUnit = .millilitres, onDone: @escaping () -> Void) {
        _model = State(initialValue: NewPartModel(store: store, babyID: babyID, unit: unit))
        self.onDone = onDone
    }

    public var body: some View {
            EntryPanel(title: PartEditCopy.newTitle, icon: icon, tint: tint, canSave: model.canSave,
                       identifierPrefix: "partEdit", onCancel: onDone, onSave: save, onDelete: nil) {
                PanelSection {
                    Picker(PartEditCopy.kindLabel, selection: $model.kind) {
                        Text(PartEditCopy.breastTitle).tag(PartKind.breast)
                        Text(PartEditCopy.bottleTitle).tag(PartKind.bottle)
                        Text(Tracker.sleep.title).tag(PartKind.sleep)
                    }
                    .pickerStyle(.segmented)
                    .accessibilityIdentifier("partEdit.newKind")
                }
                PartFields(model: model, isShowingDetails: $isShowingDetails)
            }
    }

    private var icon: BabyIcon {
        switch model.kind {
        case .breast: model.side == .left ? .breastLeft : .breastRight
        case .bottle: .bottle
        case .sleep: .sleep
        }
    }

    private var tint: Color { model.kind == .sleep ? Palette.sleep : Palette.feed }

    private func save() {
        do {
            try model.save()
            errorReporter.clear()
            onDone()
        } catch {
            let subject = model.kind == .sleep ? ErrorSubject.sleep : ErrorSubject.feed
            errorReporter.report(ErrorBannerCopy.saveFailed(what: subject), error: error, category: AppLog.store)
        }
    }
}
