package migrate

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

func GenerateManifest(source fs.FS, min, max int64) (Compatibility, error) {
	if source == nil {
		return Compatibility{}, fmt.Errorf("migration source is required")
	}
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return Compatibility{}, fmt.Errorf("read migration source: %w", err)
	}
	known := make([]MigrationDigest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return Compatibility{}, err
		}
		contents, err := fs.ReadFile(source, path.Clean(entry.Name()))
		if err != nil {
			return Compatibility{}, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		digest := sha256.Sum256(contents)
		known = append(known, MigrationDigest{Version: version, Name: name, SHA256: fmt.Sprintf("%x", digest)})
	}
	sort.Slice(known, func(i, j int) bool { return known[i].Version < known[j].Version })
	compatibility := Compatibility{Min: min, Max: max, Known: known}
	if err := validateCompatibilityCatalog(compatibility); err != nil {
		return Compatibility{}, err
	}
	return compatibility, nil
}

func VerifyManifestExtension(previous, next Compatibility) error {
	if err := validateCompatibilityCatalog(previous); err != nil {
		return fmt.Errorf("previous manifest: %w", err)
	}
	if err := validateCompatibilityCatalog(next); err != nil {
		return fmt.Errorf("next manifest: %w", err)
	}
	if len(next.Known) != len(previous.Known) && len(next.Known) != len(previous.Known)+1 {
		return fmt.Errorf("manifest extension must add exactly one contiguous migration")
	}
	for index, item := range previous.Known {
		if next.Known[index] != item {
			return fmt.Errorf("historical migration %d changed", item.Version)
		}
	}
	if len(next.Known) == len(previous.Known) && (next.Min != previous.Min || next.Max != previous.Max) {
		return fmt.Errorf("same-version regeneration changed compatibility bounds")
	}
	return nil
}

func VerifyManifestHistory(previous, next Compatibility) error {
	if err := validateCompatibilityCatalog(previous); err != nil {
		return fmt.Errorf("previous manifest: %w", err)
	}
	if err := validateCompatibilityCatalog(next); err != nil {
		return fmt.Errorf("next manifest: %w", err)
	}
	if len(previous.Known) != len(next.Known) {
		return fmt.Errorf("prepared overlap manifests must describe the same migration history")
	}
	for index, item := range previous.Known {
		if next.Known[index] != item {
			return fmt.Errorf("historical migration %d changed", item.Version)
		}
	}
	return nil
}
