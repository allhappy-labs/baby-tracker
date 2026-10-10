package export

import (
	"bytes"
	"encoding/json"
	"testing"
)

type goldenExport struct {
	SectionOrder []string                   `json:"section_order"`
	Sections     map[string]json.RawMessage `json:"sections"`
}

// splitArchive splits the encoded archive object into its sections, keeping
// each section's exact bytes and the order they were written in.
func splitArchive(t *testing.T, raw []byte) goldenExport {
	t.Helper()
	result := goldenExport{SectionOrder: []string{}, Sections: map[string]json.RawMessage{}}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("archive does not open an object: %v %v", token, err)
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		name, ok := token.(string)
		if !ok {
			t.Fatalf("archive key %v", token)
		}
		var section json.RawMessage
		if err := decoder.Decode(&section); err != nil {
			t.Fatal(err)
		}
		if _, duplicate := result.Sections[name]; duplicate {
			t.Fatalf("archive section %s twice", name)
		}
		result.SectionOrder = append(result.SectionOrder, name)
		result.Sections[name] = section
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		t.Fatalf("archive does not close: %v %v", token, err)
	}
	if decoder.More() {
		t.Fatal("trailing archive bytes")
	}
	return result
}
