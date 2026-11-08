import SwiftUI
import Charts
import BabyUI

/// One section: icon, per-day average and its neutral change, a bar chart,
/// and the detail rows.
struct InsightSectionCard: View {
    let section: InsightSection
    @State private var selected: String?
    @Environment(\.dynamicTypeSize) private var typeSize
    /// One footnote line, for the chart's gridline labels.
    @ScaledMetric(relativeTo: .footnote) private var gridLabelLine: CGFloat = 18

    var body: some View {
        Card(padding: Spacing.m) {
            content
        }
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("insights.section." + section.id)
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: Spacing.s) {
            headline
            if section.headline == nil {
                // The headline's label already says it; read once.
                Text(L10n.tr("Nothing logged in this period."))
                    .textRole(.caption)
                    .accessibilityHidden(true)
            } else {
                chart
                axis
                if section.kind == .sleep { sleepKey }
                VStack(alignment: .leading, spacing: 0) { ForEach(section.rows) { rowView($0) } }
            }
        }
    }

    private var headline: some View {
        let isLarge = typeSize.isAccessibilitySize
        let layout = isLarge
            ? AnyLayout(VStackLayout(alignment: .leading, spacing: 4))
            : AnyLayout(HStackLayout(alignment: .firstTextBaseline, spacing: 8))
        return layout {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                TrackerIcon(section.tracker).foregroundStyle(section.tracker.color)
                if let headline = section.headline {
                    Text(headline).numeralStyle(.display).foregroundStyle(Palette.primaryText)
                    if !isLarge { unitText }
                }
            }
            // At the accessibility sizes the unit gets a line of its own:
            // beside the figure it squeezed "7 год 45 хв" onto three lines
            // and left "годувань /" dangling above "день".
            if isLarge, section.headline != nil { unitText }
            if !isLarge { Spacer(minLength: 0) }
            if let change = section.change {
                VStack(alignment: isLarge ? .leading : .trailing, spacing: 0) {
                    Text(change.value).font(.footnote.monospacedDigit())
                    if !change.caption.isEmpty { Text(change.caption).font(.footnote) }
                }
                .foregroundStyle(Palette.secondaryText)
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(section.accessibilityLabel)
    }

    private var unitText: some View {
        Text(section.unit).font(.subheadline.weight(.semibold)).foregroundStyle(Palette.secondaryText)
    }

    private var selectedBar: ChartBar? { section.bars.first { $0.id == selected && !$0.isEmpty } }

    /// The toggle rule behind a tap on the chart: a tap on the bar already
    /// selected clears it, a tap elsewhere selects that bar, and a tap that
    /// missed every bar (an empty bar, an unresolved x value, or an id this
    /// chart doesn't have) leaves the current selection untouched. Static and
    /// pure so it is testable without rendering the chart.
    static func toggledSelection(tapped: String?, current: String?, bars: [ChartBar]) -> String? {
        guard let tapped, let bar = bars.first(where: { $0.id == tapped }), !bar.isEmpty else { return current }
        return bar.id == current ? nil : bar.id
    }

    private var chart: some View {
        Chart {
            ForEach(section.bars) { bar in
                BarMark(x: .value("Day", bar.id), y: .value(section.title, bar.primary))
                    .foregroundStyle(section.tracker.color.opacity(bar.isToday ? 0.3 : 0.75))
                    .cornerRadius(3)
                    .accessibilityLabel(bar.label)
                    .accessibilityValue(bar.valueText)
                if bar.secondary > 0 {
                    BarMark(x: .value("Day", bar.id), y: .value(L10n.tr("Naps"), bar.secondary))
                        .foregroundStyle(section.tracker.color.opacity(bar.isToday ? 0.15 : 0.35))
                        .cornerRadius(3)
                        .accessibilityHidden(true)
                }
            }
            if let average = section.average {
                RuleMark(y: .value("Average", average))
                    .lineStyle(StrokeStyle(lineWidth: 1, dash: [4, 3]))
                    .foregroundStyle(Palette.primaryText.opacity(0.4))
                    .accessibilityHidden(true)
            }
            if let bar = selectedBar {
                RuleMark(x: .value("Day", bar.id))
                    .foregroundStyle(Palette.primaryText.opacity(0.2))
                    .annotation(position: .top, overflowResolution: .init(x: .fit(to: .chart), y: .disabled)) {
                        Text(bar.callout).font(.footnote.monospacedDigit())
                            .padding(6).background(Palette.ground.opacity(0.95), in: .rect(cornerRadius: Radius.control))
                    }
                    .accessibilityHidden(true)
            }
        }
        .chartOverlay { proxy in
            // `.chartXSelection` resets its binding to nil on finger-up, so a
            // tap only flashed the callout; a tap gesture over the plot area
            // keeps the selection until the next tap. Mirrors `GrowthChart`.
            GeometryReader { geometry in
                Rectangle().fill(.clear).contentShape(Rectangle())
                    .onTapGesture { location in
                        guard let plotFrame = proxy.plotFrame else { return }
                        let origin = geometry[plotFrame].origin
                        let tapped = proxy.value(atX: location.x - origin.x, as: String.self)
                        selected = Self.toggledSelection(tapped: tapped, current: selected, bars: section.bars)
                    }
            }
            .accessibilityHidden(true)
        }
        .chartXAxis(.hidden)
        .chartYScale(domain: 0 ... (section.gridlines.last ?? 1))
        .chartYAxis {
            AxisMarks(position: .trailing, values: section.gridlines) { value in
                AxisGridLine()
                AxisValueLabel {
                    if let v = value.as(Double.self), let i = section.gridlines.firstIndex(of: v) {
                        Text(section.gridLabels[i]).font(.footnote)
                    }
                }
            }
        }
        // Tall enough that the two gridline labels, half the height apart,
        // never touch: at the accessibility sizes the footnote outgrows the
        // diapers chart's 56 pt.
        .frame(height: max(section.kind == .diapers ? 56 : 84, gridLabelLine * 2.4))
        .accessibilityIdentifier("insights.chart." + section.id)
    }

    private var axis: some View {
        HStack {
            Text(section.axisStart)
            Spacer(minLength: 8)
            Text(section.axisEnd)
        }
        .font(.footnote)
        .foregroundStyle(Palette.secondaryText)
        .accessibilityElement(children: .combine)
    }

    private var sleepKey: some View {
        HStack(spacing: Spacing.m) {
            key(L10n.tr("Night"), opacity: 0.75)
            key(L10n.tr("Naps"), opacity: 0.35)
        }
        .font(.footnote)
        .foregroundStyle(Palette.secondaryText)
        .accessibilityElement(children: .combine)
    }

    private func key(_ title: String, opacity: Double) -> some View {
        HStack(spacing: 4) {
            Circle().fill(section.tracker.color.opacity(opacity)).frame(width: 9, height: 9)
            Text(title)
        }
    }

    @ViewBuilder
    private func rowView(_ row: InsightRow) -> some View {
        VStack(alignment: .leading, spacing: Spacing.xs) {
            ListRow(row.label, value: row.value, density: .compact)
            if !row.shares.isEmpty {
                GeometryReader { geometry in
                    HStack(spacing: 0) {
                        ForEach(Array(row.shares.enumerated()), id: \.offset) { index, share in
                            Rectangle().fill(section.tracker.color.opacity([1, 0.6, 0.35][min(index, 2)]))
                                .frame(width: geometry.size.width * share)
                        }
                    }
                }
                .frame(height: 6)
                .clipShape(Capsule())
            }
        }
        // The row is its own accessibility element, so its 44 pt target is
        // set here, inside it, not by the container around it.
        .cardRow()
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(row.label): \(row.value)")
    }
}
