import BabyCore
import BabyStore
import BabySync
import BabyUI
import SwiftUI

/// What is about to be shared, before it is shared.
///
/// Joining a family moves this device's existing records into it, and that is
/// not obvious from a button labelled "Join". Showing the counts first — and
/// the birth dates as the user reads them — is what makes it an informed
/// decision rather than a surprise.
public struct ImportSummaryView: View {
    public struct Summary: Sendable, Equatable {
        public let babyNames: [String]
        public let displayedBirthDates: [String]
        public let recordCount: Int

        public init(babyNames: [String], displayedBirthDates: [String], recordCount: Int) {
            self.babyNames = babyNames
            self.displayedBirthDates = displayedBirthDates
            self.recordCount = recordCount
        }

        public var isEmpty: Bool { babyNames.isEmpty && recordCount == 0 }
    }

    private let summary: Summary
    private let onContinue: () -> Void
    private let onCancel: () -> Void

    public init(summary: Summary, onContinue: @escaping () -> Void, onCancel: @escaping () -> Void) {
        self.summary = summary
        self.onContinue = onContinue
        self.onCancel = onCancel
    }

    public var body: some View {
        SettingsPage {
            CardSection(ImportSummaryCopy.header) {
                ForEach(Array(summary.babyNames.enumerated()), id: \.offset) { index, name in
                    ListRow(name, value: summary.displayedBirthDates.indices.contains(index)
                        ? summary.displayedBirthDates[index] : "")
                }
                ListRow(ImportSummaryCopy.records, value: "\(summary.recordCount)")
                    .accessibilityIdentifier("import.recordCount")
            } footer: {
                Text(summary.isEmpty ? ImportSummaryCopy.emptyFooter : ImportSummaryCopy.footer)
            }

            CardSection {
                ActionRow(ImportSummaryCopy.continueLabel, identifier: "import.continue", action: onContinue)
                // Secondary: "Not now" beside the action it declines.
                ActionRow(ImportSummaryCopy.cancel, identifier: "import.cancel", tint: Palette.secondaryText, action: onCancel)
            }
        }
        .navigationTitle(ImportSummaryCopy.title)
    }
}

enum ImportSummaryCopy {
    static let title = String(localized: "Share your records", bundle: .module)
    static let header = String(localized: "What will be shared", bundle: .module)
    static let records = String(localized: "Entries", bundle: .module)
    static let continueLabel = String(localized: "Share and continue", bundle: .module)
    static let cancel = String(localized: "Not now", bundle: .module)
    static let footer = String(localized: "These babies and entries move into the family, where every caregiver can see and change them. Nothing is deleted from this device.", bundle: .module)
    static let emptyFooter = String(localized: "There is nothing on this device to share yet. You will start with the family's existing records.", bundle: .module)
}

public extension ImportSummaryView.Summary {
    /// Built from the FROZEN manifest, not from live rows.
    ///
    /// The user is being asked to approve exactly what will be sent, so the
    /// summary has to come from the same bytes the resolver will see. Reading
    /// live rows here would let the screen and the request disagree if
    /// anything changed between the freeze and the tap.
    static func fromFrozen(_ manifest: FrozenImportManifest) throws -> Self {
        let babies = try manifest.babies()
        return Self(
            babyNames: babies.map(\.name),
            displayedBirthDates: babies.map(\.birthDate),
            recordCount: try manifest.records().count
        )
    }
}
