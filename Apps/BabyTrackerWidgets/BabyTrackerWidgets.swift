import WidgetKit
import SwiftUI

@main
struct BabyTrackerWidgets: WidgetBundle {
    var body: some Widget {
        StatusWidget()
        QuickLogWidget()
        FeedActivityWidget()
        PumpActivityWidget()
    }
}
