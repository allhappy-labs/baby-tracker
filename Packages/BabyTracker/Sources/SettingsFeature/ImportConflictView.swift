import BabyCore
import BabyProtocol
import BabyStore
import BabySync
import BabyUI
import SwiftUI

/// The owner's decision when a local baby matches one already in the family.
///
/// Only the owner sees this, and only two answers are possible: the local baby
/// IS the family's baby, or it is a different child. There is deliberately no
/// "merge" and no automatic choice — picking wrong the automatic way merges two
/// children's records, and the rewrite that follows is a single irreversible
/// transaction.
public struct ImportConflictView: View {
    private let conflicts: [ImportConflict]
    private let babyName: (UUID) -> String
    private let onResolve: (UUID, ImportConflictResolution) -> Void

    @State private var pending: UUID?

    public init(
        conflicts: [ImportConflict],
        babyName: @escaping (UUID) -> String,
        onResolve: @escaping (UUID, ImportConflictResolution) -> Void
    ) {
        self.conflicts = conflicts
        self.babyName = babyName
        self.onResolve = onResolve
    }

    public var body: some View {
        Form {
            if conflicts.isEmpty {
                Section {
                    Text(ImportConflictCopy.none)
                        .accessibilityIdentifier("conflict.none")
                }
            }

            ForEach(conflicts, id: \.conflictID) { conflict in
                Section {
                    ForEach(conflict.candidateBabyIDs, id: \.self) { candidate in
                        Button {
                            onResolve(
                                conflict.conflictID,
                                ImportConflictResolution(choice: .useExisting, canonicalBabyID: candidate)
                            )
                        } label: {
                            VStack(alignment: .leading, spacing: 2) {
                                Text(ImportConflictCopy.useExisting)
                                Text(babyName(candidate))
                                    .font(.footnote)
                                    .foregroundStyle(Palette.secondaryText)
                            }
                            .frame(minHeight: 44)
                        }
                        .accessibilityIdentifier("conflict.useExisting.\(candidate.uuidString)")
                    }

                    Button {
                        onResolve(
                            conflict.conflictID,
                            ImportConflictResolution(choice: .keepLocal, canonicalBabyID: nil)
                        )
                    } label: {
                        Text(ImportConflictCopy.keepLocal).frame(minHeight: 44)
                    }
                    .accessibilityIdentifier("conflict.keepLocal.\(conflict.conflictID.uuidString)")
                } header: {
                    Text(babyName(conflict.localBabyID))
                } footer: {
                    Text(ImportConflictCopy.explanation)
                }
            }
        }
        .navigationTitle(ImportConflictCopy.title)
    }
}

enum ImportConflictCopy {
    static let title = String(localized: "Is this the same baby?", bundle: .module)
    static let useExisting = String(localized: "Same baby — use the family's records", bundle: .module)
    static let keepLocal = String(localized: "A different baby — add separately", bundle: .module)
    static let none = String(localized: "Nothing needs your decision.", bundle: .module)
    static let explanation = String(localized: "The family already has a baby with this name and birth date. Choosing \"same baby\" combines your entries with theirs. Choosing \"different baby\" adds yours as another child. This cannot be undone here.", bundle: .module)
}
