import Foundation
import BabyCore

/// What a tap on a sleep control should do.
///
/// The same intent backs three controls that look nothing alike:
///
/// - the Home Screen widget's **Sleep** button, a genuine toggle — with
///   nothing running, the tap means "start a nap";
/// - the sleep Live Activity's **Wake up** button, which is not a toggle at
///   all;
/// - the sleep break card's **Sleep** button, which is not one either.
///
/// A Live Activity can outlive the thing it describes: the nap is ended in the
/// app, the activity lingers on the Lock Screen, and a tap on a button
/// labelled **Wake up** arrives with nothing to stop. Treating that as a
/// toggle silently starts a *new* nap — a button that says Wake up starting a
/// sleep session, recorded against a real child. The break card's **Sleep**
/// has the mirror-image hazard: another caregiver started the nap a minute
/// ago, and a toggle would end it.
///
/// So the decision lives here, in one place with a test, rather than as an
/// `if` inside an intent the test suite cannot reach.
public enum SleepToggleAction: Equatable, Sendable {
    /// End the running nap.
    case stop
    /// Begin a nap.
    case start
    /// Do nothing at all — a Wake up with nothing to stop, or a Sleep with
    /// a nap already running.
    case nothing

    /// - Parameters:
    ///   - hasRunningSleep: whether a sleep timer is actually running.
    ///   - isStopControl: whether the tap came from a control labelled Wake
    ///     up, i.e. a running card's. It may never start anything.
    ///   - isStartControl: whether the tap came from a break card's Sleep. It
    ///     may never stop anything.
    public static func decide(
        hasRunningSleep: Bool,
        isStopControl: Bool,
        isStartControl: Bool = false
    ) -> SleepToggleAction {
        if hasRunningSleep { return isStartControl ? .nothing : .stop }
        return isStopControl ? .nothing : .start
    }
}
