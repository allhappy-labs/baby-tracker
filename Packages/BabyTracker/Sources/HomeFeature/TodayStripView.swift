import SwiftUI
import BabyCore
import BabyUI

/// Midnight to now: sleep as bars, feeds as ticks, and the day's
/// joined-session totals beneath. A running sleep or feed grows at the right
/// edge from the strip's own minute timeline. Tapping it opens History.
struct TodayStripView: View {
    let strip: TodayStrip
    let volumeUnit: VolumeUnit
    let onOpenHistory: () -> Void

    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    /// The totals row's width, and the width it needs with the pumped
    /// amount in it: the amount shows only where the whole row fits.
    @State private var rowWidth: CGFloat = 0
    @State private var rowWidthWithPump: CGFloat = 0
    @ScaledMetric(relativeTo: .caption2) private var axisHeight: CGFloat = 13
    /// How far an hour label keeps from the bar's edges, clear of "00" and "now".
    @ScaledMetric(relativeTo: .caption2) private var axisEdgeRoom: CGFloat = 40

    var body: some View {
        Button(action: onOpenHistory) {
            TimelineView(.everyMinute) { context in
                Card {
                    VStack(alignment: .leading, spacing: 10) {
                        // A caption, not `ListRow`'s body-sized title: the
                        // bar is what this card says. The chevron is the one
                        // `ListRow` draws for a disclosure.
                        HStack {
                            Text(HomeCopy.today)
                                .textRole(.caption)
                            Spacer(minLength: 0)
                            Image(systemName: "chevron.right")
                                .font(.footnote.weight(.semibold))
                                .foregroundStyle(Palette.secondaryText)
                                .accessibilityHidden(true)
                        }
                        VStack(spacing: Spacing.xs) {
                            bar(now: context.date)
                            // At the accessibility sizes the labels cannot sit
                            // side by side; the bar reads without them.
                            if !dynamicTypeSize.isAccessibilitySize {
                                axis(now: context.date)
                            }
                        }
                        totals
                    }
                }
                .contentShape(Rectangle())
            }
        }
        .buttonStyle(.plain)
        .accessibilityLabel(spokenTotals)
        .accessibilityIdentifier("home.todayStrip")
    }

    private func bar(now: Date) -> some View {
        GeometryReader { proxy in
            let width = proxy.size.width
            let x = { (date: Date) in position(of: date, now: now) * width }
            ZStack(alignment: .leading) {
                Rectangle().fill(Surface.card)
                ForEach(strip.hourMarks(now: now), id: \.self) { mark in
                    Rectangle()
                        .fill(Surface.raised)
                        .frame(width: 1)
                        .offset(x: x(mark))
                }
                ForEach(Array(strip.sleeps.enumerated()), id: \.offset) { _, sleep in
                    Rectangle()
                        .fill(Palette.sleep.opacity(0.7))
                        .frame(width: max(x(sleep.end) - x(sleep.start), 2))
                        .offset(x: x(sleep.start))
                }
                if let since = strip.runningSleepSince {
                    Rectangle()
                        .fill(Palette.sleep)
                        .frame(width: max(width - x(since), 3))
                        .offset(x: min(x(since), width - 3))
                }
                ForEach(Array(strip.feeds.enumerated()), id: \.offset) { _, feed in
                    Rectangle()
                        .fill(Palette.feed)
                        .frame(width: 2)
                        .offset(x: min(x(feed), width - 2))
                }
                if let since = strip.runningFeedSince {
                    Rectangle()
                        .fill(Palette.feed)
                        .frame(width: max(width - x(since), 4))
                        .shadow(color: Palette.feed.opacity(0.8), radius: 4)
                        .offset(x: min(x(since), width - 4))
                }
            }
        }
        .frame(height: 28)
        .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
        .accessibilityHidden(true)
    }

    /// Midnight at the left edge, "now" at the right, and the six-hourly
    /// marks between — each dropped when it would crowd an edge label.
    private func axis(now: Date) -> some View {
        GeometryReader { proxy in
            let width = proxy.size.width
            ZStack(alignment: .topLeading) {
                axisLabel(HomeCopy.hour(strip.dayStart))
                ForEach(strip.hourMarks(now: now), id: \.self) { mark in
                    let x = position(of: mark, now: now) * width
                    if x > axisEdgeRoom, x < width - axisEdgeRoom {
                        axisLabel(HomeCopy.hour(mark))
                            .fixedSize()
                            .frame(width: 0)
                            .offset(x: x)
                    }
                }
                axisLabel(HomeCopy.now)
                    .frame(maxWidth: .infinity, alignment: .trailing)
            }
        }
        .frame(height: axisHeight)
        .accessibilityHidden(true)
    }

    private func axisLabel(_ text: String) -> some View {
        Text(text)
            .font(.caption2.monospacedDigit())
            .foregroundStyle(Palette.secondaryText)
            .lineLimit(1)
    }

    /// One row, or a column at the accessibility sizes. One view tree either
    /// way: swapping trees (`ViewThatFits`) reads to the accessibility audit
    /// as text that cannot change size.
    ///
    /// The pumped amount is the row's last item, and only where the row has
    /// room for it: a hidden copy of the row with it measures the width it
    /// needs. It never takes a second line. The column always has room.
    private var totals: some View {
        let stacked = dynamicTypeSize.isAccessibilitySize
        let showsPump = pumpText != nil && (stacked || rowWidthWithPump <= rowWidth)
        return totalsRow(totalItems(withPump: showsPump), stacked: stacked)
            .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { rowWidth = $0 }
            .background(alignment: .leading) {
                if pumpText != nil, !stacked {
                    totalsRow(totalItems(withPump: true), stacked: false)
                        .fixedSize()
                        .hidden()
                        .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { rowWidthWithPump = $0 }
                }
            }
    }

    private func totalsRow(_ items: [TotalItem], stacked: Bool) -> some View {
        let layout = stacked
            ? AnyLayout(VStackLayout(alignment: .leading, spacing: 8))
            : AnyLayout(HStackLayout(spacing: 12))
        return layout {
            ForEach(Array(items.enumerated()), id: \.offset) { index, item in
                if index > 0, !stacked { Spacer(minLength: 0) }
                total(item)
            }
        }
    }

    private struct TotalItem {
        let icon: BabyIcon
        let color: Color
        let value: String
    }

    /// Feeds, sleep and diapers always; bottle volume only once there is
    /// some; then the pumped amount, where it fits.
    private func totalItems(withPump: Bool) -> [TotalItem] {
        var items = [
            TotalItem(icon: Tracker.feed.icon, color: Tracker.feed.color, value: String(strip.feedCount)),
            TotalItem(icon: Tracker.sleep.icon, color: Tracker.sleep.color, value: ElapsedFormatter.total(strip.sleepSeconds)),
            TotalItem(icon: Tracker.diaper.icon, color: Tracker.diaper.color, value: String(strip.diaperCount)),
        ]
        if let volume = volumeText {
            items.append(TotalItem(icon: .bottle, color: Palette.feed, value: volume))
        }
        if withPump, let pumped = pumpText {
            items.append(TotalItem(icon: Tracker.pump.icon, color: Tracker.pump.color, value: pumped))
        }
        return items
    }

    private var volumeText: String? {
        strip.volumeML > 0 ? Volume(millilitres: strip.volumeML).formatted(as: volumeUnit) : nil
    }

    /// Today's pumped total, the same on every baby's Home.
    private var pumpText: String? {
        strip.showsPump ? Volume(millilitres: strip.pumpML).formatted(as: volumeUnit) : nil
    }

    private func total(_ item: TotalItem) -> some View {
        HStack(spacing: 5) {
            BabyIconView(item.icon, size: 16)
                .foregroundStyle(item.color)
            Text(item.value)
                .font(.subheadline.weight(.semibold).monospacedDigit())
                .foregroundStyle(Palette.primaryText)
        }
    }

    /// Where `date` falls between midnight and now, from 0 to 1.
    private func position(of date: Date, now: Date) -> CGFloat {
        let span = max(now.timeIntervalSince(strip.dayStart), 1)
        return CGFloat(min(max(date.timeIntervalSince(strip.dayStart) / span, 0), 1))
    }

    /// "Today: Feed 3, Sleep 4 hours, 10 minutes, Diaper 2, Bottle 120 ml".
    private var spokenTotals: String {
        HomeCopy.today + ": " + ([
            Tracker.feed.title + " \(strip.feedCount)",
            Tracker.sleep.title + " " + HomeCopy.spokenDuration(TimeInterval(strip.sleepSeconds)),
            Tracker.diaper.title + " \(strip.diaperCount)",
        ] + (volumeText.map { [HomeCopy.bottle + " " + $0] } ?? [])
          + (pumpText.map { [Tracker.pump.title + " " + $0] } ?? [])).joined(separator: ", ")
    }
}
