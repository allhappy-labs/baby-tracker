package export

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

type testChunks []Chunk

func (c *testChunks) Put(_ context.Context, v Chunk) error { *c = append(*c, v); return nil }
func (c testChunks) Each(_ context.Context, visit func(Chunk) error) error {
	for _, v := range c {
		if err := visit(v); err != nil {
			return err
		}
	}
	return nil
}

func TestEnvelopeExactMultipleGetsEmptyFinalChunk(t *testing.T) {
	core := HeaderCore{ExportID: "00000000-0000-4000-8000-000000000001", InstanceID: "00000000-0000-4000-8000-000000000002", FamilyID: "00000000-0000-4000-8000-000000000003", RequesterAccountID: "00000000-0000-4000-8000-000000000004", KeyID: "key"}
	key := bytes.Repeat([]byte{3}, 32)
	input := bytes.Repeat([]byte("x"), EnvelopeChunkBytes)
	var chunks testChunks
	header, err := SealArchive(context.Background(), core, key, bytes.NewReader(input), &chunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0].Final || !chunks[1].Final || chunks[1].PlaintextLength != 0 {
		t.Fatalf("frames: %+v", chunks)
	}
	var out bytes.Buffer
	if err := OpenArchive(context.Background(), header, key, chunks, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), input) {
		t.Fatal("round trip changed data")
	}
}

func TestEnvelopeRejectsTamperingAndWrongKey(t *testing.T) {
	core := HeaderCore{ExportID: "00000000-0000-4000-8000-000000000001", KeyID: "key"}
	key := bytes.Repeat([]byte{3}, 32)
	var chunks testChunks
	header, err := SealArchive(context.Background(), core, key, strings.NewReader("sensitive"), &chunks)
	if err != nil {
		t.Fatal(err)
	}
	if err := OpenArchive(context.Background(), header, bytes.Repeat([]byte{4}, 32), chunks, &bytes.Buffer{}); err == nil {
		t.Fatal("wrong key accepted")
	}
	altered := append(testChunks{}, chunks...)
	altered[0].Ciphertext = append([]byte{}, altered[0].Ciphertext...)
	altered[0].Ciphertext[0] ^= 1
	if err := OpenArchive(context.Background(), header, key, altered, &bytes.Buffer{}); err == nil {
		t.Fatal("altered ciphertext accepted")
	}
	header.Core.FamilyID = "changed"
	if err := OpenArchive(context.Background(), header, key, chunks, &bytes.Buffer{}); err == nil {
		t.Fatal("altered header accepted")
	}
}

func TestEnvelopeRejectsMissingFrame(t *testing.T) {
	core := HeaderCore{ExportID: "00000000-0000-4000-8000-000000000001", KeyID: "key"}
	key := bytes.Repeat([]byte{3}, 32)
	var chunks testChunks
	header, err := SealArchive(context.Background(), core, key, bytes.NewReader(bytes.Repeat([]byte{1}, EnvelopeChunkBytes)), &chunks)
	if err != nil {
		t.Fatal(err)
	}
	if err := OpenArchive(context.Background(), header, key, chunks[:1], &bytes.Buffer{}); err == nil {
		t.Fatal("missing final frame accepted")
	}
}

func TestEnvelopeEmptyStreamAndCancelledContext(t *testing.T) {
	core := HeaderCore{ExportID: "00000000-0000-4000-8000-000000000001", KeyID: "key"}
	key := bytes.Repeat([]byte{3}, 32)
	var chunks testChunks
	header, err := SealArchive(context.Background(), core, key, strings.NewReader(""), &chunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || !chunks[0].Final || chunks[0].PlaintextLength != 0 || header.Core.PlaintextBytes != 0 {
		t.Fatalf("empty stream framing: %+v", chunks)
	}
	var out bytes.Buffer
	if err := OpenArchive(context.Background(), header, key, chunks, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("empty stream emitted data")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SealArchive(cancelled, core, key, strings.NewReader(""), &testChunks{}); err != context.Canceled {
		t.Fatalf("seal cancellation: %v", err)
	}
	if err := OpenArchive(cancelled, header, key, chunks, &out); err != context.Canceled {
		t.Fatalf("open cancellation: %v", err)
	}
}

func TestEnvelopeRejectsDuplicateAndTrailingFrames(t *testing.T) {
	core := HeaderCore{ExportID: "00000000-0000-4000-8000-000000000001", KeyID: "key"}
	key := bytes.Repeat([]byte{3}, 32)
	var chunks testChunks
	header, err := SealArchive(context.Background(), core, key, strings.NewReader("a"), &chunks)
	if err != nil {
		t.Fatal(err)
	}
	duplicated := testChunks{chunks[0], chunks[0]}
	if err := OpenArchive(context.Background(), header, key, duplicated, &bytes.Buffer{}); err == nil {
		t.Fatal("trailing duplicate accepted")
	}
	changedLength := testChunks{chunks[0]}
	changedLength[0].PlaintextLength = 2
	if err := OpenArchive(context.Background(), header, key, changedLength, &bytes.Buffer{}); err == nil {
		t.Fatal("changed length accepted")
	}
}
