package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRunWritesVerifiedManifestAtomically(t *testing.T) {
	directory := t.TempDir()
	migrations := filepath.Join(directory, "migrations")
	if err := os.Mkdir(migrations, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrations, "000001_a.sql"), []byte("SELECT 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(directory, "previous.json")
	if err := os.WriteFile(previous, []byte("{\n  \"min\": 1,\n  \"max\": 1,\n  \"known\": [{\"version\":1,\"name\":\"000001_a.sql\",\"sha256\":\"b4e0497804e46e0a0b0b8c31975b062152d551bac49c3c2e80932567b4085dcd\"}]\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrations, "000002_b.sql"), []byte("SELECT 2;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "compatibility.json")
	if err := run([]string{"--migrations", migrations, "--previous", previous, "--output", output, "--min", "1", "--max", "2"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"version": 2`)) || len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("output is not canonical indented JSON: %s", data)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "migrations" && entry.Name() != "previous.json" && entry.Name() != "compatibility.json" {
			t.Fatalf("temporary file leaked: %s", entry.Name())
		}
	}
}
