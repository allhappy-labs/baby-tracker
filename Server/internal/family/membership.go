package family

// Allowed is the family role policy: whether a membership role may perform
// an action. It is the single source of truth for every family
// authorization: server composition passes it as authority.Options.Allowed,
// and the authority evaluates it (with the family_sync entitlement) in every
// guarded transaction and integration read.
func Allowed(role, action string) bool {
	// Reading is the whole point of a viewer, so it is granted to all three
	// roles. Writing is not: a viewer reaching a write action is refused here
	// and, for record mutations specifically, refused again inside the sync
	// transaction so the client gets the precise read_only_caregiver code
	// rather than a generic membership error.
	if action == "read" || action == "read_receipt" {
		return role == "owner" || role == "caregiver" || role == "viewer"
	}
	if action == "mutate_record" || action == "add_baby" {
		return role == "owner" || role == "caregiver"
	}
	switch action {
	case "delete_baby", "manage_members", "repair_profile", "transfer_owner", "export", "export_family", "delete_family":
		return role == "owner"
	default:
		return false
	}
}
