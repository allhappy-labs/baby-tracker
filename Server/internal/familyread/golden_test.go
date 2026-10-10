package familyread

import (
	"encoding/json"
	"testing"
	"time"

	"babytracker/server/internal/testkit"
)

type goldenFamilyRead struct {
	Babies      json.RawMessage `json:"list_babies"`
	History     json.RawMessage `json:"history"`
	HistoryPage json.RawMessage `json:"history_page_two"`
	Summary     json.RawMessage `json:"summary"`
}

// goldenHistoryLimit splits baby A's September history over two pages.
const goldenHistoryLimit = 6

// newGoldenFixture is newFixture without its baby: the golden scenario
// creates every record itself.
func newGoldenFixture(t *testing.T) *fixture {
	t.Helper()
	h := testkit.NewAtFixtureClock(t) // the golden reads were recorded at it
	reader := h.IntegrationReader(t)
	f := &fixture{h: h, reader: reader, key: fixedKey(0x42), now: h.Now}
	f.guard = &countingGuard{inner: reader}
	f.service = New(f.guard, f.key, func() time.Time { return f.now })
	return f
}
