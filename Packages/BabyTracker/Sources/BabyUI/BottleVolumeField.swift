import SwiftUI
import BabyCore

/// An editable number beside the bottle stepper. The draft stays as text while
/// typing so a trailing decimal separator is not lost between keystrokes.
public struct BottleVolumeField: View {
    /// `.inline` sits in a row beside a label; `.display` is the amount
    /// between a bottle's − and +, as large as the number it replaces.
    public enum Style: Sendable {
        case inline, display
    }

    @Binding private var volumeML: Double
    private let unit: VolumeUnit
    private let label: String
    private let style: Style
    @State private var draft = ""
    @State private var lastTypedVolumeML: Double?
    /// The whole amount is selected when the field is tapped, so typing
    /// replaces it: a cursor dropped mid-number made "75" into "7560".
    @State private var selection: TextSelection?
    @FocusState private var isFocused: Bool

    public init(volumeML: Binding<Double>, unit: VolumeUnit, label: String, style: Style = .inline) {
        _volumeML = volumeML
        self.unit = unit
        self.label = label
        self.style = style
    }

    public var body: some View {
        switch style {
        case .inline:
            HStack(spacing: 4) {
                editor
                    .multilineTextAlignment(.leading)
                    .monospacedDigit()
                    .frame(minWidth: 48, maxWidth: 80, alignment: .leading)
                Text(unit.shortName)
                    .foregroundStyle(.secondary)
            }
            .font(.title3)
        case .display:
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                // Sized by the digits, not the field: a text field's own
                // width counts its placeholder, which pushed "90 ml" off
                // centre. The field lies over the digits it measures.
                Text(draft.isEmpty ? "0" : draft)
                    .hidden()
                    .overlay {
                        editor.multilineTextAlignment(.center)
                    }
                    .numeralStyle(.display)
                Text(unit.shortName)
                    .font(.title3)
                    .foregroundStyle(Palette.secondaryText)
            }
        }
    }

    private var editor: some View {
        numberField
        .focused($isFocused)
        .accessibilityLabel(label)
        .onChange(of: draft) { _, text in
            guard isFocused else { return }
            guard let number = Double(text.replacingOccurrences(of: ",", with: ".")),
                  number.isFinite, number >= 0 else {
                // Clearing the field must not leave the previous amount
                // ready to save behind an empty draft.
                lastTypedVolumeML = 0
                volumeML = 0
                return
            }
            let enteredML = unit == .millilitres ? number : Volume(ounces: number).millilitres
            lastTypedVolumeML = enteredML
            volumeML = enteredML
        }
        .onChange(of: volumeML) { _, newValue in
            if !isFocused || newValue != lastTypedVolumeML { refresh() }
        }
        .onChange(of: isFocused) { _, focused in
            if focused {
                selection = TextSelection(range: draft.startIndex..<draft.endIndex)
            } else {
                refresh()
            }
        }
        .onAppear(perform: refresh)
    }

    @ViewBuilder private var numberField: some View {
        #if os(iOS)
        TextField(label, text: $draft, selection: $selection).keyboardType(.decimalPad)
        #else
        TextField(label, text: $draft, selection: $selection)
        #endif
    }

    private func refresh() {
        let value = unit == .millilitres ? volumeML : Volume(millilitres: volumeML).ounces
        draft = value.formatted(.number.grouping(.never).precision(.fractionLength(0...2)))
    }
}
