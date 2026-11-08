package loadtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"

	"babytracker/server/internal/protocol"
)

// The light pages (pullPage, snapshotPage) must class every answer as the
// full decode does: the same outcome (ok, protocol or cross_family) and,
// when ok, the same high water and next page token. These tests compare the
// two over random pages, valid and broken in every way the protocol's
// decoder checks, read through readResponse as the generator reads them.

// readAs reads raw as a 200 answer into out, as client.do does.
func readAs(t *testing.T, raw []byte, out any) result {
	t.Helper()
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw)), ContentLength: -1}
	return readResponse(t.Context(), response, out)
}

type classed struct {
	outcome   string
	highWater uint64
	next      string
}

func (c classed) String() string {
	return fmt.Sprintf("%s hw=%d next=%q", c.outcome, c.highWater, c.next)
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// classFull is the full decode's class of a pull page for familyID.
func classFull(t *testing.T, raw []byte, familyID string) classed {
	var response protocol.SyncResponse
	res := readAs(t, raw, &response)
	if res.ok() && !(&generator{}).ownFamily(&device{DeviceState: DeviceState{FamilyID: familyID}}, response) {
		res.Outcome = OutcomeCrossFamily
	}
	if !res.ok() {
		return classed{outcome: res.Outcome}
	}
	return classed{outcome: res.Outcome, highWater: response.HighWater.Sequence, next: deref(response.NextPageToken)}
}

// classLight is pullPage's class of a pull page for familyID.
func classLight(t *testing.T, raw []byte, familyID string) classed {
	var page pullPage
	res := readAs(t, raw, &page)
	if res.ok() && !page.ownFamily(familyID) {
		res.Outcome = OutcomeCrossFamily
	}
	if !res.ok() {
		return classed{outcome: res.Outcome}
	}
	return classed{outcome: res.Outcome, highWater: page.HighWater.Sequence, next: deref(page.NextPageToken)}
}

func classSnapshotFull(t *testing.T, raw []byte, familyID string) classed {
	var page protocol.SnapshotResponse
	res := readAs(t, raw, &page)
	if res.ok() && !snapshotOwnFamily(&device{DeviceState: DeviceState{FamilyID: familyID}}, page) {
		res.Outcome = OutcomeCrossFamily
	}
	if !res.ok() {
		return classed{outcome: res.Outcome}
	}
	return classed{outcome: res.Outcome, highWater: page.HighWater.Sequence, next: deref(page.NextPageToken)}
}

func classSnapshotLight(t *testing.T, raw []byte, familyID string) classed {
	var page snapshotPage
	res := readAs(t, raw, &page)
	if res.ok() && !page.ownFamily(familyID) {
		res.Outcome = OutcomeCrossFamily
	}
	if !res.ok() {
		return classed{outcome: res.Outcome}
	}
	return classed{outcome: res.Outcome, highWater: page.HighWater.Sequence, next: deref(page.NextPageToken)}
}

// pageFixture is a family's changes and entities as the server encodes
// them, generic (json.Number keeps every integer exact) so a case can
// break them.
type pageFixture struct {
	instance, family, other string
	changes                 []map[string]any
	entities                []map[string]any
	outcomes                []map[string]any
}

func generic(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func newPageFixture(t *testing.T) *pageFixture {
	t.Helper()
	g := testGenerator(t, newFakeV1(t), 2)
	d := g.devices[0]
	fx := &pageFixture{instance: g.header.ServerInstanceID, family: d.FamilyID, other: g.devices[4].FamilyID}
	for seq := 1; seq <= 60; seq++ {
		p := g.planMutation(d)
		o := fakeOutcome(fx.instance, d.FamilyID, p.m, uint64(seq))
		entity := *o.CanonicalEntity
		operation := p.m.Operation
		if seq%7 == 0 && entity.Meta.TombstonedAt == nil {
			accepted := entity.Meta.AcceptedAt
			entity.Payload, entity.Meta.TombstonedAt, operation = nil, &accepted, "delete"
		}
		change := protocol.JournalChange{Cursor: *o.Cursor, EntityKind: entity.EntityKind, EntityID: entity.Meta.ID,
			EntityVersion: entity.Meta.EntityVersion, ActorAccountID: entity.Meta.LastEditorAccountID, DeviceID: d.DeviceID,
			Operation: operation, AcceptedAt: entity.Meta.AcceptedAt, SnapshotOrTombstone: entity}
		fx.changes = append(fx.changes, generic(t, change))
		fx.entities = append(fx.entities, generic(t, entity))
		if seq <= 3 {
			fx.outcomes = append(fx.outcomes, generic(t, o))
		}
	}
	return fx
}

func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = clone(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = clone(e)
		}
		return out
	}
	return v
}

// breaker breaks one thing of a page (top is the page, item one of its
// changes or entities, nil when it has none).
type breaker struct {
	name string
	top  func(fx *pageFixture, page map[string]any)
	item func(fx *pageFixture, item map[string]any)
}

func sub(m map[string]any, key string) map[string]any {
	out, _ := m[key].(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

const unsafeInteger = "9007199254740992" // MaxSafeInteger + 1

// entityBreakers break an entity (a change's snapshot_or_tombstone, or a
// snapshot page's entity).
func entityBreakers() []breaker {
	set := func(path ...string) func(value any) func(*pageFixture, map[string]any) {
		return func(value any) func(*pageFixture, map[string]any) {
			return func(_ *pageFixture, m map[string]any) {
				for _, key := range path[:len(path)-1] {
					m = sub(m, key)
				}
				m[path[len(path)-1]] = value
			}
		}
	}
	del := func(path ...string) func(*pageFixture, map[string]any) {
		return func(_ *pageFixture, m map[string]any) {
			for _, key := range path[:len(path)-1] {
				m = sub(m, key)
			}
			delete(m, path[len(path)-1])
		}
	}
	return []breaker{
		{name: "kind unknown", item: set("entity_kind")("nap")},
		{name: "kind number", item: set("entity_kind")(json.Number("3"))},
		{name: "kind null", item: set("entity_kind")(nil)},
		{name: "kind missing", item: del("entity_kind")},
		{name: "meta null", item: set("meta")(nil)},
		{name: "meta missing", item: del("meta")},
		{name: "meta empty", item: set("meta")(map[string]any{})},
		{name: "meta string", item: set("meta")("x")},
		{name: "meta version 0", item: set("meta", "entity_version")(json.Number("0"))},
		{name: "meta version unsafe", item: set("meta", "entity_version")(json.Number(unsafeInteger))},
		{name: "meta version negative", item: set("meta", "entity_version")(json.Number("-1"))},
		{name: "meta version string", item: set("meta", "entity_version")("2")},
		{name: "meta family other", item: func(fx *pageFixture, m map[string]any) { sub(m, "meta")["family_id"] = fx.other }},
		{name: "meta family number", item: set("meta", "family_id")(json.Number("1"))},
		{name: "tombstoned", item: set("meta", "tombstoned_at")("2026-10-25T10:00:00.000000Z")},
		{name: "not tombstoned", item: set("meta", "tombstoned_at")(nil)},
		{name: "tombstoned number", item: set("meta", "tombstoned_at")(json.Number("1"))},
		{name: "payload null", item: set("payload")(nil)},
		{name: "payload missing", item: del("payload")},
		{name: "payload number", item: set("payload")(json.Number("5"))},
		{name: "payload string", item: set("payload")("{}")},
		{name: "payload array", item: set("payload")([]any{})},
	}
}

// pullBreakers break a pull page or one of its changes.
func pullBreakers() []breaker {
	var out []breaker
	for _, b := range entityBreakers() {
		inner := b.item
		out = append(out, breaker{name: "entity " + b.name, item: func(fx *pageFixture, m map[string]any) {
			if e, ok := m["snapshot_or_tombstone"].(map[string]any); ok {
				inner(fx, e)
			}
		}})
	}
	change := func(f func(fx *pageFixture, m map[string]any)) func(*pageFixture, map[string]any) { return f }
	out = append(out,
		breaker{name: "change kind unknown", item: change(func(_ *pageFixture, m map[string]any) { m["entity_kind"] = "nap" })},
		breaker{name: "change kind other known", item: change(func(_ *pageFixture, m map[string]any) { m["entity_kind"] = "immunization" })},
		breaker{name: "change kind number", item: change(func(_ *pageFixture, m map[string]any) { m["entity_kind"] = json.Number("1") })},
		breaker{name: "both kinds unknown", item: change(func(_ *pageFixture, m map[string]any) {
			m["entity_kind"] = "nap"
			sub(m, "snapshot_or_tombstone")["entity_kind"] = "nap"
		})},
		breaker{name: "optional", item: change(func(_ *pageFixture, m map[string]any) { m["optional"] = true })},
		breaker{name: "optional string", item: change(func(_ *pageFixture, m map[string]any) { m["optional"] = "true" })},
		breaker{name: "snapshot null", item: change(func(_ *pageFixture, m map[string]any) { m["snapshot_or_tombstone"] = nil })},
		breaker{name: "snapshot missing", item: change(func(_ *pageFixture, m map[string]any) { delete(m, "snapshot_or_tombstone") })},
		breaker{name: "version 0", item: change(func(_ *pageFixture, m map[string]any) { m["entity_version"] = json.Number("0") })},
		breaker{name: "version unsafe", item: change(func(_ *pageFixture, m map[string]any) { m["entity_version"] = json.Number(unsafeInteger) })},
		breaker{name: "version string", item: change(func(_ *pageFixture, m map[string]any) { m["entity_version"] = "x" })},
		breaker{name: "version negative", item: change(func(_ *pageFixture, m map[string]any) { m["entity_version"] = json.Number("-1") })},
		breaker{name: "version fraction", item: change(func(_ *pageFixture, m map[string]any) { m["entity_version"] = json.Number("1.5") })},
		breaker{name: "version null", item: change(func(_ *pageFixture, m map[string]any) { m["entity_version"] = nil })},
		breaker{name: "cursor family other", item: change(func(fx *pageFixture, m map[string]any) { sub(m, "cursor")["family_id"] = fx.other })},
		breaker{name: "cursor family bad", item: change(func(_ *pageFixture, m map[string]any) { sub(m, "cursor")["family_id"] = "F0000001" })},
		breaker{name: "cursor server zero", item: change(func(_ *pageFixture, m map[string]any) {
			sub(m, "cursor")["server_instance_id"] = "00000000-0000-0000-0000-000000000000"
		})},
		breaker{name: "cursor server upper", item: change(func(fx *pageFixture, m map[string]any) {
			sub(m, "cursor")["server_instance_id"] = strings.ToUpper(fx.instance)
		})},
		breaker{name: "cursor server other", item: change(func(_ *pageFixture, m map[string]any) {
			sub(m, "cursor")["server_instance_id"] = "0b1c2d3e-4f5a-4b6c-8d7e-9f0a1b2c3d4e"
		})},
		breaker{name: "cursor epoch 0", item: change(func(_ *pageFixture, m map[string]any) { sub(m, "cursor")["recovery_epoch"] = json.Number("0") })},
		breaker{name: "cursor sequence unsafe", item: change(func(_ *pageFixture, m map[string]any) {
			sub(m, "cursor")["sequence"] = json.Number(unsafeInteger)
		})},
		breaker{name: "cursor null", item: change(func(_ *pageFixture, m map[string]any) { m["cursor"] = nil })},
		breaker{name: "cursor missing", item: change(func(_ *pageFixture, m map[string]any) { delete(m, "cursor") })},
		breaker{name: "server bad", top: func(_ *pageFixture, p map[string]any) { p["server_instance_id"] = "nope" }},
		breaker{name: "server missing", top: func(_ *pageFixture, p map[string]any) { delete(p, "server_instance_id") }},
		breaker{name: "epoch 0", top: func(_ *pageFixture, p map[string]any) { p["recovery_epoch"] = json.Number("0") }},
		breaker{name: "epoch unsafe", top: func(_ *pageFixture, p map[string]any) { p["recovery_epoch"] = json.Number(unsafeInteger) }},
		breaker{name: "outcomes null", top: func(_ *pageFixture, p map[string]any) { p["outcomes"] = nil }},
		breaker{name: "outcomes missing", top: func(_ *pageFixture, p map[string]any) { delete(p, "outcomes") }},
		breaker{name: "outcome other family", top: func(fx *pageFixture, p map[string]any) {
			o := clone(fx.outcomes[0]).(map[string]any)
			sub(o, "canonical_entity")["meta"].(map[string]any)["family_id"] = fx.other
			p["outcomes"] = []any{o}
		}},
		breaker{name: "outcome own", top: func(fx *pageFixture, p map[string]any) { p["outcomes"] = []any{clone(fx.outcomes[1])} }},
		breaker{name: "outcome bad cursor", top: func(fx *pageFixture, p map[string]any) {
			o := clone(fx.outcomes[2]).(map[string]any)
			sub(o, "cursor")["family_id"] = "x"
			p["outcomes"] = []any{o}
		}},
		breaker{name: "changes null", top: func(_ *pageFixture, p map[string]any) { p["changes"] = nil }},
		breaker{name: "changes missing", top: func(_ *pageFixture, p map[string]any) { delete(p, "changes") }},
		breaker{name: "changes object", top: func(_ *pageFixture, p map[string]any) { p["changes"] = map[string]any{} }},
		breaker{name: "high water family other", top: func(fx *pageFixture, p map[string]any) { sub(p, "high_water")["family_id"] = fx.other }},
		breaker{name: "high water bad", top: func(_ *pageFixture, p map[string]any) { sub(p, "high_water")["family_id"] = "x" }},
		breaker{name: "high water missing", top: func(_ *pageFixture, p map[string]any) { delete(p, "high_water") }},
		breaker{name: "next page", top: func(_ *pageFixture, p map[string]any) { p["next_page_token"] = "page-2" }},
		breaker{name: "next page number", top: func(_ *pageFixture, p map[string]any) { p["next_page_token"] = json.Number("2") }},
		breaker{name: "extra key", top: func(_ *pageFixture, p map[string]any) { p["newer_field"] = map[string]any{"a": 1} }},
	)
	return out
}

// backslash is written so: an escape sequence in this file's source would
// be read for the character it names by some editors.
var backslash = string(rune(92))

// byteBreakers break the encoded page itself.
var byteBreakers = []struct {
	name  string
	apply func(raw []byte, rng *rand.Rand) []byte
}{
	{"none", func(raw []byte, _ *rand.Rand) []byte { return raw }},
	{"truncated", func(raw []byte, rng *rand.Rand) []byte { return raw[:rng.IntN(len(raw))] }},
	{"trailing garbage", func(raw []byte, _ *rand.Rand) []byte { return append(raw, []byte(" x")...) }},
	{"escaped kind", func(raw []byte, _ *rand.Rand) []byte {
		return bytes.Replace(raw, []byte(`"entity_kind":"feed"`), []byte(`"entity_kind":"`+backslash+`u0066eed"`), 1)
	}},
	{"escaped inner kind", func(raw []byte, _ *rand.Rand) []byte {
		// An entity's own kind (its keys sort entity_kind, meta, payload).
		return bytes.Replace(raw, []byte(`"entity_kind":"sleep","meta"`), []byte(`"entity_kind":"sl`+backslash+`u0065ep","meta"`), 1)
	}},
	{"bad escape in kind", func(raw []byte, _ *rand.Rand) []byte {
		return bytes.Replace(raw, []byte(`"entity_kind":"diaper"`), []byte(`"entity_kind":"dia`+backslash+`qper"`), 1)
	}},
	{"upper-case keys", func(raw []byte, _ *rand.Rand) []byte {
		raw = bytes.ReplaceAll(raw, []byte(`"family_id"`), []byte(`"FAMILY_ID"`))
		return bytes.ReplaceAll(raw, []byte(`"entity_kind"`), []byte(`"Entity_Kind"`))
	}},
	{"whitespace", func(raw []byte, _ *rand.Rand) []byte {
		raw = bytes.ReplaceAll(raw, []byte(`":`), []byte("\" :\n "))
		return bytes.ReplaceAll(raw, []byte(`null`), []byte(" null "))
	}},
}

func encodePage(t *testing.T, page map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLightPullPageClassesEveryAnswerAsTheFullDecode(t *testing.T) {
	fx := newPageFixture(t)
	breakers := pullBreakers()
	rng := rand.New(rand.NewPCG(21, 4))
	counts, changed := map[string]int{}, map[string]int{}
	for i := range 6000 {
		n := rng.IntN(6)
		changes := make([]any, n)
		from := rng.IntN(len(fx.changes) - n + 1)
		for j := range n {
			changes[j] = clone(fx.changes[from+j])
		}
		page := map[string]any{"server_instance_id": fx.instance, "recovery_epoch": json.Number("1"), "outcomes": []any{},
			"changes": changes, "high_water": map[string]any{"server_instance_id": fx.instance, "recovery_epoch": json.Number("1"),
				"family_id": fx.family, "sequence": json.Number(fmt.Sprint(from + n))}, "next_page_token": nil}
		var names []string
		target := -1 // the item the previous item breaker hit
		for range rng.IntN(5) {
			b := breakers[rng.IntN(len(breakers))]
			names = append(names, b.name)
			switch {
			case b.top != nil:
				b.top(fx, page)
			case n > 0:
				items, _ := page["changes"].([]any)
				if len(items) > 0 {
					if target < 0 || target >= len(items) || rng.IntN(2) == 0 {
						target = rng.IntN(len(items))
					}
					if m, ok := items[target].(map[string]any); ok {
						b.item(fx, m)
					}
				}
			}
		}
		bb := byteBreakers[rng.IntN(len(byteBreakers))]
		names = append(names, bb.name)
		encoded := encodePage(t, page)
		raw := bb.apply(encoded, rng)
		if !bytes.Equal(raw, encoded) {
			changed[bb.name]++
		}
		full, light := classFull(t, raw, fx.family), classLight(t, raw, fx.family)
		if full != light {
			t.Fatalf("case %d %v: the full decode classes it %v, the light page %v\n%s", i, names, full, light, raw)
		}
		counts[full.outcome]++
	}
	// The cases reach every class, so the comparison means something.
	for _, outcome := range []string{OutcomeOK, OutcomeProtocol, OutcomeCrossFamily} {
		if counts[outcome] < 50 {
			t.Fatalf("only %d cases classed %s: %v", counts[outcome], outcome, counts)
		}
	}
	// And every byte breaker but "none" changed some pages.
	for _, bb := range byteBreakers[1:] {
		if changed[bb.name] < 20 {
			t.Fatalf("the byte breaker %q changed only %d pages", bb.name, changed[bb.name])
		}
	}
	t.Logf("classes: %v; pages each byte breaker changed: %v", counts, changed)
}

func TestLightSnapshotPageClassesEveryAnswerAsTheFullDecode(t *testing.T) {
	fx := newPageFixture(t)
	breakers := entityBreakers()
	top := []breaker{
		{name: "server bad", top: func(_ *pageFixture, p map[string]any) { p["server_instance_id"] = "nope" }},
		{name: "snapshot id bad", top: func(_ *pageFixture, p map[string]any) { p["snapshot_id"] = "00000000-0000-0000-0000-000000000000" }},
		{name: "snapshot id missing", top: func(_ *pageFixture, p map[string]any) { delete(p, "snapshot_id") }},
		{name: "epoch 0", top: func(_ *pageFixture, p map[string]any) { p["recovery_epoch"] = json.Number("0") }},
		{name: "entities null", top: func(_ *pageFixture, p map[string]any) { p["entities"] = nil }},
		{name: "entities missing", top: func(_ *pageFixture, p map[string]any) { delete(p, "entities") }},
		{name: "high water family other", top: func(fx *pageFixture, p map[string]any) { sub(p, "high_water")["family_id"] = fx.other }},
		{name: "high water bad", top: func(_ *pageFixture, p map[string]any) { sub(p, "high_water")["recovery_epoch"] = json.Number("0") }},
		{name: "next page", top: func(_ *pageFixture, p map[string]any) { p["next_page_token"] = "page-2" }},
	}
	breakers = append(breakers, top...)
	rng := rand.New(rand.NewPCG(22, 5))
	counts, changed := map[string]int{}, map[string]int{}
	for i := range 4000 {
		n := rng.IntN(6)
		entities := make([]any, n)
		from := rng.IntN(len(fx.entities) - n + 1)
		for j := range n {
			entities[j] = clone(fx.entities[from+j])
		}
		page := map[string]any{"server_instance_id": fx.instance, "recovery_epoch": json.Number("1"),
			"snapshot_id": "5e1f0c2a-9b3d-4c7e-8f6a-1d2b3c4e5f60", "entities": entities,
			"high_water": map[string]any{"server_instance_id": fx.instance, "recovery_epoch": json.Number("1"), "family_id": fx.family,
				"sequence": json.Number("60")}, "next_page_token": nil}
		var names []string
		target := -1 // the item the previous item breaker hit
		for range rng.IntN(5) {
			b := breakers[rng.IntN(len(breakers))]
			names = append(names, b.name)
			switch {
			case b.top != nil:
				b.top(fx, page)
			case n > 0:
				items, _ := page["entities"].([]any)
				if len(items) > 0 {
					if target < 0 || target >= len(items) || rng.IntN(2) == 0 {
						target = rng.IntN(len(items))
					}
					if m, ok := items[target].(map[string]any); ok {
						b.item(fx, m)
					}
				}
			}
		}
		bb := byteBreakers[rng.IntN(len(byteBreakers))]
		names = append(names, bb.name)
		encoded := encodePage(t, page)
		raw := bb.apply(encoded, rng)
		if !bytes.Equal(raw, encoded) {
			changed[bb.name]++
		}
		full, light := classSnapshotFull(t, raw, fx.family), classSnapshotLight(t, raw, fx.family)
		if full != light {
			t.Fatalf("case %d %v: the full decode classes it %v, the light page %v\n%s", i, names, full, light, raw)
		}
		counts[full.outcome]++
	}
	for _, outcome := range []string{OutcomeOK, OutcomeProtocol, OutcomeCrossFamily} {
		if counts[outcome] < 50 {
			t.Fatalf("only %d cases classed %s: %v", counts[outcome], outcome, counts)
		}
	}
	// And every byte breaker but "none" changed some pages.
	for _, bb := range byteBreakers[1:] {
		if changed[bb.name] < 20 {
			t.Fatalf("the byte breaker %q changed only %d pages", bb.name, changed[bb.name])
		}
	}
	t.Logf("classes: %v; pages each byte breaker changed: %v", counts, changed)
}

// A skipped change (optional, of a kind the protocol does not know) is held
// to what the protocol reads of it and no more: whatever its entity_version
// and its entity's meta are, the page is ok both ways; the same change
// applied (optional false) is a protocol answer both ways.
func TestLightPullPageSkipsWhatTheProtocolSkips(t *testing.T) {
	fx := newPageFixture(t)
	type edit func(map[string]any)
	kinds := map[string]edit{
		"both kinds unknown": func(c map[string]any) {
			c["entity_kind"] = "nap"
			sub(c, "snapshot_or_tombstone")["entity_kind"] = "nap"
		},
		"change kind unknown": func(c map[string]any) { c["entity_kind"] = "nap" },
		"entity kind unknown": func(c map[string]any) { sub(c, "snapshot_or_tombstone")["entity_kind"] = "nap" },
		"kinds differ":        func(c map[string]any) { c["entity_kind"] = "immunization" },
	}
	breaks := map[string]edit{
		"entity_version x":  func(c map[string]any) { c["entity_version"] = "x" },
		"entity_version -1": func(c map[string]any) { c["entity_version"] = json.Number("-1") },
		"meta x":            func(c map[string]any) { sub(c, "snapshot_or_tombstone")["meta"] = "x" },
		"meta null":         func(c map[string]any) { sub(c, "snapshot_or_tombstone")["meta"] = nil },
		"meta.family_id 1":  func(c map[string]any) { sub(sub(c, "snapshot_or_tombstone"), "meta")["family_id"] = json.Number("1") },
		"meta.tombstoned_at 1": func(c map[string]any) {
			sub(sub(c, "snapshot_or_tombstone"), "meta")["tombstoned_at"] = json.Number("1")
		},
		"meta.entity_version x":  func(c map[string]any) { sub(sub(c, "snapshot_or_tombstone"), "meta")["entity_version"] = "x" },
		"payload null, no stone": func(c map[string]any) { sub(c, "snapshot_or_tombstone")["payload"] = nil },
		"all of them": func(c map[string]any) {
			c["entity_version"] = "x"
			sub(c, "snapshot_or_tombstone")["meta"] = "x"
			sub(c, "snapshot_or_tombstone")["payload"] = json.Number("1")
		},
	}
	for kindName, kind := range kinds {
		for breakName, brk := range breaks {
			for _, optional := range []bool{true, false} {
				change := clone(fx.changes[0]).(map[string]any)
				kind(change)
				brk(change)
				change["optional"] = optional
				page := map[string]any{"server_instance_id": fx.instance, "recovery_epoch": json.Number("1"), "outcomes": []any{},
					"changes": []any{change, clone(fx.changes[1])}, "high_water": map[string]any{"server_instance_id": fx.instance,
						"recovery_epoch": json.Number("1"), "family_id": fx.family, "sequence": json.Number("2")}, "next_page_token": nil}
				raw := encodePage(t, page)
				full, light := classFull(t, raw, fx.family), classLight(t, raw, fx.family)
				want := OutcomeProtocol
				if optional {
					want = OutcomeOK
				}
				if full != light || full.outcome != want {
					t.Errorf("%s, %s, optional %v: full %v, light %v, want %s", kindName, breakName, optional, full, light, want)
				}
			}
		}
	}
}

// What the light pages do not check, by design: a payload's shape against
// its kind, and the type of a field nothing reads. The full decode refuses
// both; an untracked device accepts them. (A tracked device decodes in
// full, and the protocol package's own tests hold the server to the
// shapes.)
func TestLightPagesLeaveThePayloadShapeToTheFullDecode(t *testing.T) {
	fx := newPageFixture(t)
	for _, tc := range []struct {
		name  string
		apply func(change map[string]any)
	}{
		{"payload without its required keys", func(c map[string]any) { sub(c, "snapshot_or_tombstone")["payload"] = map[string]any{} }},
		{"operation a number", func(c map[string]any) { c["operation"] = json.Number("5") }},
		{"meta id a number", func(c map[string]any) { sub(sub(c, "snapshot_or_tombstone"), "meta")["id"] = json.Number("5") }},
		{"a kind its payload does not fit", func(c map[string]any) {
			c["entity_kind"] = "immunization"
			sub(c, "snapshot_or_tombstone")["entity_kind"] = "immunization"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			change := clone(fx.changes[0]).(map[string]any)
			if sub(change, "snapshot_or_tombstone")["payload"] == nil {
				t.Fatal("the fixture's first change is a tombstone")
			}
			tc.apply(change)
			page := map[string]any{"server_instance_id": fx.instance, "recovery_epoch": json.Number("1"), "outcomes": []any{},
				"changes": []any{change}, "high_water": map[string]any{"server_instance_id": fx.instance, "recovery_epoch": json.Number("1"),
					"family_id": fx.family, "sequence": json.Number("1")}, "next_page_token": nil}
			raw := encodePage(t, page)
			if full := classFull(t, raw, fx.family); full.outcome != OutcomeProtocol {
				t.Fatalf("the full decode classes it %v", full)
			}
			if light := classLight(t, raw, fx.family); light.outcome != OutcomeOK {
				t.Fatalf("the light page classes it %v", light)
			}
		})
	}
}

// A page read into a pooled buffer must not be changed by the next answer
// read into the same buffer: the decoded values keep no byte of it.
func TestReadResponseKeepsNoByteOfTheReusedBuffer(t *testing.T) {
	fx := newPageFixture(t)
	first := encodePage(t, map[string]any{"server_instance_id": fx.instance, "recovery_epoch": json.Number("1"), "outcomes": []any{clone(fx.outcomes[0])},
		"changes": []any{clone(fx.changes[0]), clone(fx.changes[1])}, "high_water": map[string]any{"server_instance_id": fx.instance,
			"recovery_epoch": json.Number("1"), "family_id": fx.family, "sequence": json.Number("2")}, "next_page_token": "page-2"})
	var full protocol.SyncResponse
	var light pullPage
	if res := readAs(t, first, &full); !res.ok() {
		t.Fatalf("full: %v", res)
	}
	if res := readAs(t, first, &light); !res.ok() {
		t.Fatalf("light: %v", res)
	}
	wantFull, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	wantLight := fmt.Sprintf("%+v", light)
	// Many answers of the same size through the pool overwrite every buffer.
	overwrite := bytes.Repeat([]byte("x"), len(first))
	for range 64 {
		var discard map[string]any
		_ = readAs(t, overwrite, &discard)
	}
	if gotFull, _ := json.Marshal(full); !bytes.Equal(gotFull, wantFull) {
		t.Fatalf("the full decode changed after the buffer was reused:\n%s\n%s", wantFull, gotFull)
	}
	if gotLight := fmt.Sprintf("%+v", light); gotLight != wantLight {
		t.Fatalf("the light page changed after the buffer was reused:\n%s\n%s", wantLight, gotLight)
	}
}
