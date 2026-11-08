import SwiftUI
import BabyCore
import BabyUI

/// The fields Add baby and Edit baby share: the name and birth date, then
/// the growth reference and the optional due date, as two cards.
///
/// `identifierPrefix` keeps each sheet's identifiers: `addBaby` gives
/// `addBaby.referenceSex`, `addBaby.hasDueDate` and `addBaby.dueDate`; `baby`
/// gives the same under `baby.`.
///
/// Edit baby reads the growth profile before it can show those fields:
/// until then `showsGrowth` is false and `growthPlaceholder` (a spinner, or
/// the load error and Retry) takes their card.
public struct BabyFormFields<GrowthPlaceholder: View>: View {
    @Binding private var name: String
    @Binding private var birthDate: Date
    @Binding private var referenceSex: GrowthReferenceSex?
    @Binding private var hasDueDate: Bool
    @Binding private var dueDate: Date
    private let latestBirthDate: Date?
    private let offersNoReference: Bool
    private let dueDateNeedsReference: Bool
    private let growthFooter: String?
    private let showsGrowth: Bool
    private let identifierPrefix: String
    private let growthPlaceholder: GrowthPlaceholder

    /// - Parameters:
    ///   - latestBirthDate: the last day the birth date may be; nil for no limit.
    ///   - offersNoReference: whether "Choose a reference" is one of the
    ///     picker's options (not once a reference has been saved).
    ///   - dueDateNeedsReference: the due date toggle is off until a
    ///     reference is chosen.
    public init(
        name: Binding<String>,
        birthDate: Binding<Date>,
        latestBirthDate: Date? = nil,
        referenceSex: Binding<GrowthReferenceSex?>,
        offersNoReference: Bool = true,
        hasDueDate: Binding<Bool>,
        dueDateNeedsReference: Bool = false,
        dueDate: Binding<Date>,
        growthFooter: String? = nil,
        showsGrowth: Bool = true,
        identifierPrefix: String,
        @ViewBuilder growthPlaceholder: () -> GrowthPlaceholder
    ) {
        _name = name
        _birthDate = birthDate
        _referenceSex = referenceSex
        _hasDueDate = hasDueDate
        _dueDate = dueDate
        self.latestBirthDate = latestBirthDate
        self.offersNoReference = offersNoReference
        self.dueDateNeedsReference = dueDateNeedsReference
        self.growthFooter = growthFooter
        self.showsGrowth = showsGrowth
        self.identifierPrefix = identifierPrefix
        self.growthPlaceholder = growthPlaceholder()
    }

    public var body: some View {
        CardSection {
            TextField(BabyFormCopy.name, text: $name)
                #if os(iOS)
                // Autocapitalization is a UIKit text-input concept; no macOS equivalent.
                .textInputAutocapitalization(.words)
                #endif
                .frame(minHeight: 44)
                .accessibilityLabel(BabyFormCopy.name)
            LabeledRow(BabyFormCopy.born) {
                birthDatePicker
                    .labelsHidden()
            }
        }

        CardSection {
            if showsGrowth {
                LabeledRow(BabyFormCopy.reference) {
                    // A menu with the picker in it rather than a menu-style
                    // picker: that one's button keeps a one-line height, and
                    // "Choose a reference" wrapped at the larger sizes was cut
                    // in half.
                    Menu {
                        Picker(BabyFormCopy.reference, selection: $referenceSex) {
                            if offersNoReference {
                                Text(BabyFormCopy.chooseReference).tag(GrowthReferenceSex?.none)
                            }
                            Text(BabyFormCopy.girls).tag(GrowthReferenceSex?.some(.girls))
                            Text(BabyFormCopy.boys).tag(GrowthReferenceSex?.some(.boys))
                        }
                        .pickerStyle(.inline)
                    } label: {
                        HStack(spacing: Spacing.xs) {
                            Text(referenceTitle)
                                .multilineTextAlignment(.trailing)
                                .fixedSize(horizontal: false, vertical: true)
                            Image(systemName: "chevron.up.chevron.down")
                                .font(.footnote.weight(.semibold))
                                .accessibilityHidden(true)
                        }
                        .frame(minHeight: 44)
                        .contentShape(Rectangle())
                    }
                    .accessibilityLabel(BabyFormCopy.reference)
                    .accessibilityValue(referenceTitle)
                    .accessibilityIdentifier(id("referenceSex"))
                }
                Toggle(isOn: $hasDueDate) {
                    Text(BabyFormCopy.addDueDate).fixedSize(horizontal: false, vertical: true)
                }
                .disabled(dueDateNeedsReference && referenceSex == nil)
                .accessibilityIdentifier(id("hasDueDate"))
                if hasDueDate {
                    LabeledRow(BabyFormCopy.dueDate) {
                        DatePicker(BabyFormCopy.dueDate, selection: $dueDate, displayedComponents: [.date])
                            .labelsHidden()
                            .accessibilityIdentifier(id("dueDate"))
                    }
                }
            } else {
                growthPlaceholder
            }
        } footer: {
            if showsGrowth, let growthFooter { Text(growthFooter) }
        }
    }

    private var referenceTitle: String {
        switch referenceSex {
        case .girls: BabyFormCopy.girls
        case .boys: BabyFormCopy.boys
        case nil: BabyFormCopy.chooseReference
        }
    }

    // A birth date in the future is not a correction, it is a typo — and
    // every age on Insights would go negative.
    @ViewBuilder private var birthDatePicker: some View {
        if let latestBirthDate {
            DatePicker(BabyFormCopy.born, selection: $birthDate, in: ...latestBirthDate, displayedComponents: [.date])
        } else {
            DatePicker(BabyFormCopy.born, selection: $birthDate, displayedComponents: [.date])
        }
    }

    private func id(_ field: String) -> String { babyFormIdentifier(field, prefix: identifierPrefix) }
}

public extension BabyFormFields where GrowthPlaceholder == EmptyView {
    /// Every field shown, with nothing to wait for.
    init(
        name: Binding<String>,
        birthDate: Binding<Date>,
        latestBirthDate: Date? = nil,
        referenceSex: Binding<GrowthReferenceSex?>,
        offersNoReference: Bool = true,
        hasDueDate: Binding<Bool>,
        dueDateNeedsReference: Bool = false,
        dueDate: Binding<Date>,
        growthFooter: String? = nil,
        identifierPrefix: String
    ) {
        self.init(name: name, birthDate: birthDate, latestBirthDate: latestBirthDate,
                  referenceSex: referenceSex, offersNoReference: offersNoReference,
                  hasDueDate: hasDueDate, dueDateNeedsReference: dueDateNeedsReference,
                  dueDate: dueDate, growthFooter: growthFooter, showsGrowth: true,
                  identifierPrefix: identifierPrefix, growthPlaceholder: { EmptyView() })
    }
}

extension BabyFormFields where GrowthPlaceholder == EmptyView {
    static func identifier(_ field: String, prefix: String) -> String { babyFormIdentifier(field, prefix: prefix) }
}

private func babyFormIdentifier(_ field: String, prefix: String) -> String { "\(prefix).\(field)" }

enum BabyFormCopy {
    static let name = L10n.tr("Name")
    static let born = L10n.tr("Born")
    static let reference = L10n.tr("Swiss PEZZ 2025 reference")
    static let chooseReference = L10n.tr("Choose a reference")
    static let girls = L10n.tr("Girls")
    static let boys = L10n.tr("Boys")
    static let addDueDate = L10n.tr("Add expected due date")
    static let dueDate = L10n.tr("Expected due date")
}
