// Live Activity registration on the PostgreSQL authority.
//
// A phone registers its device (push-to-start token, preferences) and each
// card's update token through Service, and withdraws through Unregistration.
// Each call is one request transaction on the authority's request seam
// (authority.RequestGuard.WithRequestTx): the session row, family policy and
// the tenant context come from the request's access token, and the
// repository binds the registration to that session's lineage
// (app.live_activity_session_binding), which replaces main's membership
// revision. Registration needs reading authority only, viewers included, and
// conveys no mutation authority. The wire DTOs are protocol's; types.go holds
// the ActivityKit payload shapes, verbatim from main.
//
// The cards are main's: presentation.go (Project) and lifecycle.go
// (Reconcile) are verbatim, and snapshot.go reads the care snapshot they
// project from care_entities (SnapshotReader), with a timer's origin and its
// stop's completion for the worker.

package liveactivity
