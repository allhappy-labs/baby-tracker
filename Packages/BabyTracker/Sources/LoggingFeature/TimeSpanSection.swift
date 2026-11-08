import SwiftUI
import BabyCore
import BabyUI

/// Start and End rows, both with the date, and the length beside them —
/// for every activity with a length. Never a duration stepper.
public struct TimeSpanSection<Footer: View>: View {
    public enum EndStyle: Sendable {
        /// Breast, sleep: an End is always there.
        case required
        /// Bottle: "Add end time" until it has one, then a ✕ to remove it.
        case optional
        /// A session still running: Start can move, End reads "Running".
        case running
    }

    /// The date and time pills stop growing here. Past it, "12 жовт. 2026 р."
    /// alone is wider than a phone and pushed the whole panel off the screen.
    private static var largestPickerSize: DynamicTypeSize { .accessibility2 }

    @Binding private var start: Date
    @Binding private var end: Date?
    private let endStyle: EndStyle
    private let latest: Date
    private let identifierPrefix: String
    private let footer: Footer

    public init(start: Binding<Date>, end: Binding<Date?>, endStyle: EndStyle, latest: Date,
                identifierPrefix: String, @ViewBuilder footer: () -> Footer) {
        _start = start
        _end = end
        self.endStyle = endStyle
        self.latest = latest
        self.identifierPrefix = identifierPrefix
        self.footer = footer()
    }

    public var body: some View {
        CardSection {
            EditTimeRow(TimeSpanCopy.start, date: $start, latest: latest, identifier: identifierPrefix + ".start")
                .dynamicTypeSize(...Self.largestPickerSize)
            if endStyle == .running {
                ListRow(TimeSpanCopy.end, value: TimeSpanCopy.running)
                    .accessibilityIdentifier(identifierPrefix + ".end")
            } else {
                endRow
                if let length = TimeSpan(start: start, end: end).lengthText {
                    ListRow(TimeSpanCopy.length, value: length)
                        .monospacedDigit()
                        .accessibilityIdentifier(identifierPrefix + ".length")
                }
            }
        } footer: {
            footer
        }
    }

    @ViewBuilder
    private var endRow: some View {
        if let end {
            EditTimeRow(TimeSpanCopy.end, date: Binding(get: { end }, set: { self.end = $0 }),
                        latest: latest, identifier: identifierPrefix + ".end") {
                if endStyle == .optional {
                    Button {
                        self.end = nil
                    } label: {
                        Image(systemName: "xmark.circle.fill")
                            .foregroundStyle(Palette.secondaryText)
                            .frame(minWidth: 44, minHeight: 44)
                    }
                    .buttonStyle(.borderless)
                    .accessibilityLabel(TimeSpanCopy.removeEnd)
                    .accessibilityIdentifier(identifierPrefix + ".removeEnd")
                }
            }
            .dynamicTypeSize(...Self.largestPickerSize)
        } else {
            Button(TimeSpanCopy.addEnd) { end = defaultEnd }
                .frame(minHeight: 44)
                .accessibilityIdentifier(identifierPrefix + ".addEnd")
        }
    }

    /// Ten minutes after the start, never past now.
    private var defaultEnd: Date { min(latest, start.addingTimeInterval(600)) }
}
