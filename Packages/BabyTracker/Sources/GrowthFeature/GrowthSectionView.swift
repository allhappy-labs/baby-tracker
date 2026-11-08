import SwiftUI
import Charts
import Observation
import BabyCore
import BabyStore
import BabyUI

/// The reference choice that used to live on the Growth details screen.
@MainActor @Observable
final class GrowthReferenceChoice {
    private let store: BabyStore
    private let babyID: UUID
    private let writeContext: RealmWriteContext?
    var errorMessage: String?

    init(store: BabyStore, babyID: UUID) {
        self.store = store
        self.babyID = babyID
        writeContext = try? store.captureWriteContext()
    }

    func choose(_ sex: GrowthReferenceSex) {
        do {
            guard let writeContext, try store.captureWriteContext() == writeContext else {
                throw GrowthStoreError.babyUnavailable
            }
            // Preserve a due date another screen may have set meanwhile.
            let due = try store.growthProfile(for: babyID)?.expectedDueDate
            try store.setGrowthProfile(for: babyID, referenceSex: sex, expectedDueDate: due)
            errorMessage = nil
        } catch {
            errorMessage = GrowthCopy.text("Couldn’t save the reference. Try again.")
        }
    }
}

/// Insights' growth section: the latest weight, length and head with their
/// Swiss percentiles, one chart from birth, and the way to the check-ins.
public struct GrowthSectionView: View {
    private let model: GrowthViewModel
    private let onOpenAllCheckIns: () -> Void
    @State private var choice: GrowthReferenceChoice
    @State private var metric: GrowthMetric = .weight
    @State private var selected: Date?
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(model: GrowthViewModel, store: BabyStore, babyID: UUID, onOpenAllCheckIns: @escaping () -> Void) {
        self.model = model
        self.onOpenAllCheckIns = onOpenAllCheckIns
        _choice = State(initialValue: GrowthReferenceChoice(store: store, babyID: babyID))
    }

    private var presentation: GrowthSectionPresentation {
        GrowthSectionPresentation(summaries: model.summaries, birthDate: model.birthDate)
    }

    public var body: some View {
        Card { content }
            .accessibilityElement(children: .contain)
            .accessibilityIdentifier("insights.growth")
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: Spacing.s) {
            HStack(alignment: .firstTextBaseline) {
                TrackerIcon(.growth).foregroundStyle(Palette.growth).accessibilityHidden(true)
                Spacer(minLength: 8)
                if let header = presentation.headerText {
                    Text(header).textRole(.caption)
                }
            }
            if presentation.isEmpty {
                Text(GrowthCopy.text("No measurements yet")).textRole(.caption)
            } else {
                chips
                if model.profileState == .missing { referenceChoice }
                if let line = presentation.changeLine(metric) {
                    Text(line).textRole(.caption).monospacedDigit().foregroundStyle(Palette.primaryText)
                }
                let summary = presentation.summary(metric)
                if summary.chartPoints.isEmpty {
                    Text(GrowthCopy.text("A trend needs two measurements.")).textRole(.caption)
                } else {
                    GrowthChart(display: GrowthMetricPresentation(summary: summary), selected: $selected).frame(height: 180)
                }
            }
            NavigationLink { GrowthReferenceInfoView() } label: {
                ListRow(GrowthCopy.text("Swiss PEZZ 2025 reference"), accessory: .disclosure, density: .compact)
                    .cardRow()
            }
            .buttonStyle(.plain)
            .accessibilityIdentifier("insights.growth.reference.info")
            if model.checkInCount > 0 {
                Button(action: onOpenAllCheckIns) {
                    ListRow(GrowthCopy.text("All check-ins"), value: "\(model.checkInCount)",
                            accessory: .disclosure, density: .compact)
                        .cardRow()
                }
                .buttonStyle(.plain)
                .accessibilityIdentifier("insights.growth.allCheckIns")
            }
            if let message = choice.errorMessage {
                InlineError(message)
            }
        }
        .onChange(of: metric) { _, _ in selected = nil }
    }

    private var chips: some View {
        let layout = typeSize.isAccessibilitySize
            ? AnyLayout(VStackLayout(spacing: 6)) : AnyLayout(HStackLayout(spacing: 6))
        return layout {
            ForEach(GrowthMetric.allCases, id: \.self) { item in
                let chip = presentation.chip(item)
                Button { metric = item } label: {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(chip.title).font(.footnote.weight(.semibold))
                        Text(chip.value).font(.body.weight(.bold).monospacedDigit())
                            .foregroundStyle(item == metric ? Palette.ground : Palette.primaryText)
                        if let percentile = chip.percentile {
                            Text(percentile).font(.footnote.weight(.bold))
                        }
                    }
                    .multilineTextAlignment(.leading)
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
                .buttonStyle(TileButtonStyle(tint: Palette.growth, size: .regular, isSelected: item == metric))
                .accessibilityLabel(chipLabel(item, chip))
                .accessibilityIdentifier("insights.growth.chip." + item.rawValue)
            }
        }
    }

    private func chipLabel(_ metric: GrowthMetric, _ chip: GrowthChip) -> String {
        let summary = presentation.summary(metric)
        guard let latest = summary.latest else { return "\(chip.title). \(GrowthCopy.text("No measurements yet"))" }
        return GrowthMetricPresentation(summary: summary).accessibilityLabel(for: latest)
    }

    private var referenceChoice: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(GrowthCopy.text("Choose the girls’ or boys’ reference for comparisons. Your recorded measurements stay available."))
                .textRole(.caption).fixedSize(horizontal: false, vertical: true)
            HStack(spacing: Spacing.s) {
                Button(GrowthCopy.text("Girls")) { choice.choose(.girls) }
                    .accessibilityIdentifier("insights.growth.reference.girls")
                Button(GrowthCopy.text("Boys")) { choice.choose(.boys) }
                    .accessibilityIdentifier("insights.growth.reference.boys")
            }
            .buttonStyle(.secondary)
        }
    }
}

/// The baby’s readings over the Swiss percentile bands and median. Moved from the old
/// details screen; selecting a point shows its date, value and percentile.
struct GrowthChart: View {
    let display: GrowthMetricPresentation
    @Binding var selected: Date?
    @Environment(\.dynamicTypeSize) private var typeSize

    private var selectedPoint: GrowthPoint? {
        guard let selected else { return nil }
        return Self.nearestPoint(to: selected, in: display.chartPoints)
    }

    private static let bandLayers: [(name: String, low: KeyPath<GrowthCentiles, Double>, high: KeyPath<GrowthCentiles, Double>)] = [
        ("p3-p97", \.p3, \.p97), ("p10-p90", \.p10, \.p90), ("p25-p75", \.p25, \.p75)
    ]

    /// The point whose `measuredAt` is closest to `date`, or nil for an
    /// empty chart.
    static func nearestPoint(to date: Date, in points: [GrowthPoint]) -> GrowthPoint? {
        points.min {
            abs($0.measuredAt.timeIntervalSince(date)) < abs($1.measuredAt.timeIntervalSince(date))
        }
    }

    /// A tap at `date` selects the nearest point's `measuredAt`; tapping the
    /// already-selected point again clears the selection.
    static func toggleSelection(tapped date: Date, in points: [GrowthPoint], current: Date?) -> Date? {
        guard let nearest = nearestPoint(to: date, in: points) else { return nil }
        return nearest.measuredAt == current ? nil : nearest.measuredAt
    }

    var body: some View {
        Chart {
            ForEach(display.referenceBands) { band in
                // Nested P3–P97, P10–P90 and P25–P75 bands; the overlaps
                // darken toward the middle, so no band hides another.
                ForEach(Self.bandLayers, id: \.name) { layer in
                    ForEach(band.samples, id: \.date) { sample in
                        AreaMark(x: .value(GrowthCopy.text("Date"), sample.date),
                                 yStart: .value("Low", sample.centiles[keyPath: layer.low]),
                                 yEnd: .value("High", sample.centiles[keyPath: layer.high]),
                                 series: .value("Reference", "\(band.id.timeIntervalSince1970)-\(layer.name)"))
                            .foregroundStyle(Palette.sleep.opacity(0.16))
                            .interpolationMethod(.monotone)
                            .accessibilityHidden(true)
                    }
                }
                ForEach(band.samples, id: \.date) { sample in
                    LineMark(x: .value(GrowthCopy.text("Date"), sample.date),
                             y: .value(GrowthCopy.text("Swiss median"), sample.centiles.p50),
                             series: .value("Reference", "\(band.id.timeIntervalSince1970)-median"))
                        .foregroundStyle(Palette.sleep).lineStyle(StrokeStyle(lineWidth: 1, dash: [4, 4]))
                        .interpolationMethod(.monotone)
                        .accessibilityHidden(true)
                }
            }
            ForEach(display.chartPoints) { point in
                LineMark(x: .value(GrowthCopy.text("Date"), point.measuredAt),
                         y: .value(display.title, point.value), series: .value("Measurements", "raw"))
                    .foregroundStyle(Palette.growth)
                    // The labelled points below are the chart's only
                    // accessible marks; the band, median, line and rule
                    // would each be read as unnamed values.
                    .accessibilityHidden(true)
                PointMark(x: .value(GrowthCopy.text("Date"), point.measuredAt), y: .value(display.title, point.value))
                    .foregroundStyle(Palette.growth)
                    .accessibilityLabel(display.dateText(point))
                    .accessibilityValue("\(display.valueText(point.value)). \(display.referenceText(point))")
            }
            if let point = selectedPoint {
                RuleMark(x: .value(GrowthCopy.text("Date"), point.measuredAt))
                    .foregroundStyle(Palette.secondaryText.opacity(0.4))
                    .annotation(position: .top, overflowResolution: .init(x: .fit(to: .chart), y: .disabled)) {
                        Text("\(display.dateText(point)) · \(display.valueText(point.value))\n\(display.referenceText(point))")
                            .font(.footnote).padding(6)
                            .background(Palette.ground.opacity(0.95), in: .rect(cornerRadius: Radius.control))
                    }
                    .accessibilityHidden(true)
            }
        }
        .chartOverlay { proxy in
            // `.chartXSelection` resets its binding to nil on finger-up, so a
            // tap only flashed the callout; a tap gesture over the plot area
            // keeps the selection until the next tap.
            GeometryReader { geometry in
                Rectangle().fill(.clear).contentShape(Rectangle())
                    .onTapGesture { location in
                        guard let plotFrame = proxy.plotFrame else { return }
                        let origin = geometry[plotFrame].origin
                        guard let date = proxy.value(atX: location.x - origin.x, as: Date.self) else { return }
                        selected = Self.toggleSelection(tapped: date, in: display.chartPoints, current: selected)
                    }
            }
            .accessibilityHidden(true)
        }
        .chartYScale(domain: display.yDomain)
        .chartXAxis {
            // At the accessibility sizes three automatic labels ("5 Oct at
            // 16") were cut to "5 жо…": two short dates, and a label that
            // would collide is dropped rather than truncated.
            if typeSize.isAccessibilitySize {
                AxisMarks(values: .automatic(desiredCount: 2)) {
                    AxisGridLine()
                    AxisValueLabel(format: .dateTime.day().month(.abbreviated), collisionResolution: .greedy)
                }
            } else {
                AxisMarks(values: .automatic(desiredCount: 3))
            }
        }
        .chartYAxis {
            AxisMarks(position: .leading, values: .automatic(desiredCount: 3))
            // The band edges are named where the curve ends; a label that
            // would collide with another is dropped rather than overlap.
            if let edge = display.trailingCentiles {
                let labelled = [(3, edge.p3), (50, edge.p50), (97, edge.p97)]
                AxisMarks(position: .trailing, values: labelled.map(\.1)) { value in
                    let rank = labelled.first { $0.1 == value.as(Double.self) }?.0
                    AxisValueLabel(collisionResolution: .greedy) {
                        if let rank { Text(GrowthCopy.format("P%@", "\(rank)")).font(.caption2) }
                    }
                    .foregroundStyle(Palette.sleep)
                }
            }
        }
        .accessibilityIdentifier("insights.growth.chart")
    }
}
