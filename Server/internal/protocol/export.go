package protocol

// CreateExportRequest starts an owner-only family data export. RequestID is
// stable across retries so a lost response cannot create another archive.
type CreateExportRequest struct {
	RequestMeta
	RequestID string `json:"request_id"`
}

type ExportStatus struct {
	ProtocolVersion string  `json:"protocol_version"`
	ExportID        string  `json:"export_id"`
	State           string  `json:"state"`
	CreatedAt       string  `json:"created_at"`
	ExpiresAt       string  `json:"expires_at"`
	PlaintextBytes  *uint64 `json:"plaintext_bytes"`
	SHA256          *string `json:"sha256"`
	Code            *string `json:"code"`
}
