import SwiftUI
import BabyCore
import BabySync
import BabyStore
import BabyUI
import BabyFeature
#if os(iOS)
import UIKit
#endif

/// Reached from the gear on Home, not from a fourth tab: the tab bar is for
/// the three things a parent logs, and settings is not one of them.
///
/// Every string it shows comes from `SettingsViewModel`.
public struct SettingsView: View {
    private let store: BabyStore
    private let selection: SelectedBabyStore
    private let settings: SettingsViewModel
    /// Optional: the app passes one, but a store-failure launch and the
    /// previews have no family graph to build one from.
    private let familyModel: FamilySettingsViewModel?
    private let onBabyDeleted: (UUID) -> Void

    @State private var babies: [Baby] = []
    /// `item:`, never `isPresented:` plus a lookup. A sheet's content closure
    /// captures the view value it was built from: the `isPresented` version of
    /// this looked up the tapped baby in `babies` and found nothing, and
    /// presented a completely blank card over Settings. `item:` hands the
    /// content the baby that was tapped, so there is nothing to go stale.
    @State private var editing: Baby?
    @Environment(\.errorReporter) private var errorReporter

    public init(
        store: BabyStore,
        selection: SelectedBabyStore,
        settings: SettingsViewModel,
        familyModel: FamilySettingsViewModel? = nil,
        onBabyDeleted: @escaping (UUID) -> Void = { _ in }
    ) {
        self.store = store
        self.selection = selection
        self.settings = settings
        self.familyModel = familyModel
        self.onBabyDeleted = onBabyDeleted
    }

    public var body: some View {
        @Bindable var settings = settings

        return NavigationStack {
            ScreenScaffold(header: .title(settings.title, identifier: "settings.title")) {
                CardSection(settings.unitsHeader) {
                    Picker(settings.unitLabel, selection: $settings.volumeUnit) {
                        ForEach(settings.unitOptions) { option in
                            Text(option.title).tag(option.value)
                        }
                    }
                    .pickerStyle(.segmented)
                    // A segmented control's own height is smaller than 44pt at
                    // the default text size, and it is the one actionable thing
                    // in this section.
                    .frame(minHeight: 44)
                    .accessibilityLabel(settings.unitLabel)
                } footer: {
                    Text(settings.unitFooter)
                }

                CardSection(settings.babiesHeader) {
                    ForEach(babies, id: \.id) { baby in
                        Button {
                            editing = baby
                        } label: {
                            ListRow(baby.name, accessory: .disclosure) {
                                BabyAvatarView(BabyAvatar(iconID: baby.iconID), size: 28)
                            } trailing: {
                                EmptyView()
                            }
                        }
                        .buttonStyle(.plain)
                        .accessibilityHint(settings.babyRowHint)
                    }
                } footer: {
                    // The normal footer tells the user to tap a baby. With the
                    // last one deleted there is nothing to tap, and leaving it
                    // there is an instruction that cannot be followed.
                    Text(babies.isEmpty ? settings.noBabiesFooter : settings.babiesFooter)
                }

                #if os(iOS)
                CardSection(SettingsCopy.appLanguage) {
                    Button {
                        if let url = URL(string: UIApplication.openSettingsURLString) {
                            Task { _ = await UIApplication.shared.open(url) }
                        }
                    } label: {
                        ListRow(SettingsCopy.changeInIPhoneSettings) {
                            EmptyView()
                        } trailing: {
                            Image(systemName: "arrow.up.right.square")
                                .foregroundStyle(Palette.secondaryText)
                                .accessibilityHidden(true)
                        }
                    }
                    .buttonStyle(.plain)
                    .accessibilityIdentifier("settings.appLanguage")
                } footer: {
                    Text(SettingsCopy.appLanguageFooter)
                }
                #endif

                // Below Babies, not above. Babies are what this screen is for; sharing
                // is secondary — and putting it above them pushed the baby
                // rows off-screen at accessibility text sizes, which the
                // Settings audit caught as a Dynamic Type regression.
                if let familyModel {
                    CardSection(SettingsCopy.sharingHeader) {
                        NavigationLink {
                            FamilySettingsView(model: familyModel)
                        } label: {
                            ListRow(SettingsCopy.caregivers, value: familySummary(familyModel), accessory: .disclosure)
                        }
                        .buttonStyle(.plain)
                        .accessibilityIdentifier("settings.caregivers")
                    } footer: {
                        Text(SettingsCopy.sharingFooter)
                    }
                }
                // After everything else rather than under Units, which is
                // where it first went: two more rows above Babies pushed
                // the baby row past the fold at the size the Settings audit
                // probes, the same regression the comment on Sharing above
                // records.
                CardSection(SettingsCopy.trackingHeader) {
                    breakStepper(SettingsCopy.joinFeedsWithin, value: $settings.feedBreakMinutes,
                                 identifier: "settings.feedBreak")
                    breakStepper(SettingsCopy.joinSleepWithin, value: $settings.sleepBreakMinutes,
                                 identifier: "settings.sleepBreak")
                    if let baby = selection.resolve(from: babies) {
                        NavigationLink {
                            HomeButtonsView(settings: settings, babyID: baby.id, babyName: baby.name)
                        } label: {
                            ListRow(HomeButtonsCopy.rowTitle, accessory: .disclosure)
                        }
                        .buttonStyle(.plain)
                        .accessibilityIdentifier("settings.homeButtons")
                    }
                }
            }
            // The pushed screens' back button says where it goes.
            .navigationTitle(settings.title)
            #if os(iOS)
            // The header is drawn by the scaffold, so the bar would leave
            // a tall empty band above it.
            .toolbar(.hidden, for: .navigationBar)
            #endif
            // No Done button. This was a sheet, and a sheet needs a way out;
            // it is a tab now, and the way out is the tab bar. A confirmation
            // action on the root of a tab implies the screen is modal and that
            // leaving it without tapping would lose something — neither of
            // which is true here: every control writes as it is touched.
        }
        // On the NavigationStack rather than on the content inside it: a sheet
        // presented from a row of a Form that is itself already inside a
        // presented sheet does not come up at all. Read through in the
        // simulator caught this — the row highlighted and nothing opened.
        .sheet(item: $editing) { baby in
            BabyEditSheet(store: store, baby: baby) {
                editing = nil
                reload()
            } onDeleted: { onBabyDeleted($0) }
        }
        .task { reload() }
        // A rename or a delete goes through `store.record`, which bumps
        // `revision`. Without this the list still shows the old name behind
        // the sheet that just changed it.
        .onChange(of: store.revision) { _, _ in reload() }
    }

    /// A gap limit: its label and value over the stepper's − and + at the
    /// accessibility sizes, beside them otherwise. The whole row is one
    /// adjustable accessibility element, so the limit is its value — what
    /// changes when it is adjusted — and every text drawn belongs to it.
    private func breakStepper(_ title: String, value: Binding<Int>, identifier: String) -> some View {
        let range = 5...120
        let step = 5
        return LabeledRow(title) {
            HStack(spacing: Spacing.s) {
                Text(SettingsCopy.minutes(value.wrappedValue))
                    .foregroundStyle(Palette.secondaryText)
                Stepper(title, value: value, in: range, step: step)
                    .labelsHidden()
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(title)
        .accessibilityValue(SettingsCopy.minutes(value.wrappedValue))
        .accessibilityAdjustableAction { direction in
            switch direction {
            case .increment: value.wrappedValue = min(value.wrappedValue + step, range.upperBound)
            case .decrement: value.wrappedValue = max(value.wrappedValue - step, range.lowerBound)
            @unknown default: break
            }
        }
        .accessibilityIdentifier(identifier)
    }

    /// The one-word state shown on the row, so a parent can see whether
    /// sharing is on without opening the screen.
    private func familySummary(_ model: FamilySettingsViewModel) -> String {
        if model.isSignedOut { return SettingsCopy.familySyncStopped }
        if model.hasFamily { return SettingsCopy.familyOn }
        return SettingsCopy.familyOff
    }

    private func reload() {
        do {
            babies = try store.babies()
            // `resolve(from:)` already falls back at read time, so the app
            // never strands on a deleted baby. This writes the fallback BACK,
            // so the id parked in the App Group stops naming a tombstoned
            // baby — otherwise the stale value outlives the deletion forever
            // and anything that reads it without resolving gets a ghost.
            if let resolved = selection.resolve(from: babies), resolved.id != selection.selectedID {
                selection.select(resolved.id)
            }
            if errorReporter.current == ErrorBannerCopy.loadFailed { errorReporter.clear() }
        } catch {
            babies = []
            errorReporter.report(
                ErrorBannerCopy.loadFailed,
                error: error,
                category: AppLog.store
            )
        }
    }
}

enum SettingsCopy {
    static let trackingHeader = String(localized: "Tracking", bundle: .module)
    static let joinFeedsWithin = String(localized: "Join feeds within", bundle: .module)
    static let joinSleepWithin = String(localized: "Join sleep within", bundle: .module)

    /// "%lld min", formatted with the device locale's digit grouping, the
    /// same way `FeedSummary` formats its own localized templates.
    static func minutes(_ value: Int) -> String {
        String(format: String(localized: "%lld min", bundle: .module), locale: .current, value)
    }

    static let appLanguage = String(localized: "App language", bundle: .module)
    static let changeInIPhoneSettings = String(localized: "Change in iPhone Settings", bundle: .module)
    static let appLanguageFooter = String(localized: "In iPhone Settings, open Apps, then Baby Tracker to choose a language.", bundle: .module)
    static let sharingHeader = String(localized: "Sharing", bundle: .module)
    static let caregivers = String(localized: "Caregivers", bundle: .module)
    static let sharingFooter = String(localized: "Share tracking with another caregiver. Baby Tracker works fully without this.", bundle: .module)
    static let familyOn = String(localized: "On", bundle: .module)
    static let familyOff = String(localized: "Off", bundle: .module)
    static let familySyncStopped = String(localized: "Stopped", bundle: .module)
}
