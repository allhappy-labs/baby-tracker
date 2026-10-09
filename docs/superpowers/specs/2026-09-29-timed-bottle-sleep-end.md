# Timed bottle and editable sleep end

## User intent

A completed sleep's end time can be corrected independently of its start. Bottle feeding can be started and stopped like a breast segment. A timed bottle can begin a feed or occur between breast segments in one feed; finishing it records volume, contents, and elapsed seconds, after which either breast can resume. Preserve the existing manual bottle log for backdated or instant entries.

## Behavior

- A completed sleep editor shows Start and End date/time fields. Changing Start alone keeps the prior duration; changing End uses the chosen time. End must be after Start and no later than the current time. Saving uses the existing sync-aware update path.
- The Feed menu and quick bottle action start a live bottle timer. Home and the Live Activity identify that a bottle is active and show its elapsed time. A breast segment closes when a bottle begins. Finish Bottle opens volume and contents input, and saves a bottle component with its actual duration. The feed stays open, so a parent can start another bottle, resume either breast, or end the feed. Stop while bottle is active first requires finishing that bottle.
- Older bottle entries have no duration and remain readable/editable. Existing manual logging remains available for after-the-fact entries.
- The timer survives app backgrounding, relaunch, and sync to the other caregiver. A v3 feed/timer payload has optional positive `seconds` on bottle components. A v3 timer has `current_bottle_started_at`, mutually exclusive with a running breast side. New timer operations start and finish that segment. Old clients must receive an explicit unsupported-capability response before cursor advance when v3 state exists.

## Boundaries

The app remains private and device-first. Do not launch either installed physical app during verification. Server deployment and installing a protocol-dependent build require an explicit final approval after the code and tests are reviewable, unless already authorized in the session.
