// Package care stores every care record (baby, feed, sleep, diaper, timer,
// supplement, growth profile, measurement, immunization, pump) as one care_entities row
// (migration 000030).
//
// Encode is the only place a canonical entity becomes a row. It writes the
// wire column as json.Marshal writes the entity, which escapes `<`, `>`, `&`,
// U+2028 and U+2029 exactly as the HTTP JSON encoder (escapeHTML on) would.
// Responses embed the stored bytes as json.RawMessage, and the encoder's
// compaction leaves already-escaped bytes unchanged, so a page served
// verbatim is byte-identical to today's decode-then-encode. The entity Encode
// receives must be canonical, as the write path and the stored-entity
// decoder build it (NFC text, normalized timestamps): the stored bytes are
// served as they are and never normalized again.
//
// Typed columns (baby, start, end, session, timer kind, sort order, label,
// name hash) filter and index; derived columns (breast seconds, bottle
// volume, observations and missing volumes, shortest timed part) are the
// write-time aggregates summaries read. A tombstone keeps its previous typed
// columns and carries no derived values.
package care
