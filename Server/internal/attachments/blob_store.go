package attachments

import (
	"context"
	"time"
)

type ObjectKey string

type Transfer struct {
	URL       string
	ExpiresAt time.Time
}

// BlobStore is the private adapter seam for a future attachment feature. The
// beta intentionally provides no implementation, route, storage, or public
// transfer DTO.
type BlobStore interface {
	AuthorizeUpload(context.Context, ObjectKey, int64, [32]byte, time.Duration) (Transfer, error)
	AuthorizeDownload(context.Context, ObjectKey, time.Duration) (Transfer, error)
	Delete(context.Context, ObjectKey) error
}
