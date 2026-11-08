import ActivityKit
import SwiftUI
import WidgetKit
import BabyCore
import BabyStore
import BabyUI

/// The pumping card: icon, "Pumping · Both", the elapsed clock and Stop.
/// Stale past six hours: no clock, no Stop, and the app ends it.
struct PumpActivityWidget: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: PumpActivityAttributes.self) { context in
            HStack(spacing: Spacing.m) {
                BabyIconView(.pump, size: 28)
                    .foregroundStyle(Palette.pump)
                VStack(alignment: .leading, spacing: 2) {
                    Text(PumpCopy.caption(context.state.side))
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(Palette.secondaryText)
                    if !context.isStale {
                        Text(context.state.startedAt, style: .timer)
                            .font(.title.weight(.bold).monospacedDigit())
                            .foregroundStyle(Palette.primaryText)
                    }
                }
                Spacer(minLength: 0)
                if !context.isStale {
                    Button(intent: StopPumpIntent(pumpID: context.attributes.pumpID)) {
                        PrimaryLabel(title: ActivityCopy.stop, fillsWidth: false)
                    }
                    .buttonStyle(.plain)
                }
            }
            .padding()
            .activityBackgroundTint(Palette.ground)
            .activitySystemActionForegroundColor(Palette.primaryText)
        } dynamicIsland: { context in
            DynamicIsland {
                DynamicIslandExpandedRegion(.leading) {
                    BabyIconView(.pump, size: 24).foregroundStyle(Palette.pump)
                }
                DynamicIslandExpandedRegion(.center) {
                    VStack(spacing: 2) {
                        Text(PumpCopy.caption(context.state.side)).font(.caption.weight(.semibold))
                        if !context.isStale {
                            Text(context.state.startedAt, style: .timer).monospacedDigit()
                        }
                    }
                }
                DynamicIslandExpandedRegion(.bottom) {
                    if !context.isStale {
                        Button(intent: StopPumpIntent(pumpID: context.attributes.pumpID)) {
                            PrimaryLabel(title: ActivityCopy.stop)
                        }
                        .buttonStyle(.plain)
                    }
                }
            } compactLeading: {
                BabyIconView(.pump, size: 18)
                    .foregroundStyle(Palette.pump)
                    .accessibilityLabel(Tracker.pump.title)
            } compactTrailing: {
                // Past six hours a clock still counting would claim the
                // session is going on.
                if !context.isStale {
                    Text(context.state.startedAt, style: .timer)
                        .monospacedDigit()
                        .frame(maxWidth: 52)
                }
            } minimal: {
                BabyIconView(.pump, size: 16).foregroundStyle(Palette.pump)
            }
        }
    }
}
