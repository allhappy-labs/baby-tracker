import SwiftUI
import BabyCore

/// An editable number beside the bottle stepper. The draft stays as text while
/// typing so a trailing decimal separator is not lost between keystrokes.
public struct BottleVolumeField: View {
    @Binding private var volumeML: Double
    private let unit: VolumeUnit
    private let label: String
    @State private var draft = ""
    @State private var lastTypedVolumeML: Double?
    @FocusState private var isFocused: Bool

    public init(volumeML: Binding<Double>, unit: VolumeUnit, label: String) {
        _volumeML = volumeML
        self.unit = unit
        self.label = label
    }

    public var body: some View {
        HStack(spacing: 4) {
            numberField
                .multilineTextAlignment(.leading)
                .monospacedDigit()
                .frame(minWidth: 48, maxWidth: 80, alignment: .leading)
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
                    if !focused { refresh() }
                }
            Text(unit.shortName)
                .foregroundStyle(.secondary)
        }
        .font(.title3)
        .onAppear(perform: refresh)
    }

    @ViewBuilder private var numberField: some View {
        #if os(iOS)
        TextField(label, text: $draft).keyboardType(.decimalPad)
        #else
        TextField(label, text: $draft)
        #endif
    }

    private func refresh() {
        let value = unit == .millilitres ? volumeML : Volume(millilitres: volumeML).ounces
        draft = value.formatted(.number.grouping(.never).precision(.fractionLength(0...2)))
    }
}
