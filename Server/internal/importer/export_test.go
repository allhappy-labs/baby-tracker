package importer

// PublicationMutationID is the mutation id of the publication of
// localBabyID by import importID.
func PublicationMutationID(importID, localBabyID string) string {
	return deterministicID(importID, localBabyID, "publication")
}
