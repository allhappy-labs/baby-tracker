package migrate

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type MigrationDigest struct {
	Version int64  `json:"version"`
	Name    string `json:"name"`
	SHA256  string `json:"sha256"`
}

type Compatibility struct {
	Min   int64             `json:"min"`
	Max   int64             `json:"max"`
	Known []MigrationDigest `json:"known"`
}

//go:embed compatibility.json
var embeddedCompatibility []byte

func EmbeddedCompatibility() (Compatibility, error) {
	var compatibility Compatibility
	decoder := json.NewDecoder(strings.NewReader(string(embeddedCompatibility)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&compatibility); err != nil {
		return Compatibility{}, fmt.Errorf("decode embedded migration compatibility: %w", err)
	}
	if err := validateCompatibilityCatalog(compatibility); err != nil {
		return Compatibility{}, err
	}
	return compatibility, nil
}

func ValidateCompatibility(applied []MigrationDigest, compatibility Compatibility) error {
	if err := validateCompatibilityCatalog(compatibility); err != nil {
		return err
	}
	if len(applied) == 0 {
		return errors.New("migration_compatibility: no applied migrations")
	}
	known := make(map[int64]MigrationDigest, len(compatibility.Known))
	for _, item := range compatibility.Known {
		known[item.Version] = item
	}
	seen := make(map[int64]bool, len(applied))
	var current int64
	for _, item := range applied {
		if item.Version < 1 || seen[item.Version] {
			return fmt.Errorf("migration_compatibility: duplicate or invalid applied version %d", item.Version)
		}
		seen[item.Version] = true
		expected, ok := known[item.Version]
		if !ok || item.Name != expected.Name || item.SHA256 != expected.SHA256 {
			return fmt.Errorf("migration_compatibility: unknown or changed migration %d", item.Version)
		}
		if item.Version > current {
			current = item.Version
		}
	}
	// The history is complete from the first known version: 1, or the baseline.
	for version := compatibility.Known[0].Version; version <= current; version++ {
		if !seen[version] {
			return fmt.Errorf("migration_compatibility: missing applied version %d", version)
		}
	}
	if current < compatibility.Min || current > compatibility.Max {
		return fmt.Errorf("migration_compatibility: current version %d outside [%d,%d]", current, compatibility.Min, compatibility.Max)
	}
	return nil
}

// validateCompatibilityCatalog checks the known history: versions 1, 2, … or
// a squashed baseline (NNNNNN_baseline.sql at version B) followed by B+1, …,
// with min and max inside it.
func validateCompatibilityCatalog(compatibility Compatibility) error {
	if len(compatibility.Known) == 0 {
		return errors.New("migration_compatibility: invalid compatibility bounds")
	}
	first := compatibility.Known[0].Version
	if first != 1 && !isBaseline(compatibility.Known[0].Name) {
		return fmt.Errorf("migration_compatibility: invalid known migration %d", first)
	}
	last := first + int64(len(compatibility.Known)) - 1
	if compatibility.Min < first || compatibility.Max < compatibility.Min || last < compatibility.Max {
		return errors.New("migration_compatibility: invalid compatibility bounds")
	}
	for index, item := range compatibility.Known {
		version := first + int64(index)
		parsedVersion, parsedName, err := parseMigrationName(item.Name)
		if item.Version != version || err != nil || parsedVersion != item.Version || parsedName != item.Name {
			return fmt.Errorf("migration_compatibility: invalid known migration %d", item.Version)
		}
		digest, err := hex.DecodeString(item.SHA256)
		if err != nil || len(digest) != 32 || fmt.Sprintf("%x", digest) != item.SHA256 {
			return fmt.Errorf("migration_compatibility: invalid digest for version %d", item.Version)
		}
	}
	return nil
}

func (r *Runner) CheckCompatibility(ctx context.Context, pool *pgxpool.Pool, compatibility Compatibility) error {
	_, err := r.CompatibleVersion(ctx, pool, compatibility)
	return err
}

// CompatibleVersion validates the complete applied migration history and
// returns the exact schema version whose privilege snapshot may be trusted.
func (r *Runner) CompatibleVersion(ctx context.Context, pool *pgxpool.Pool, compatibility Compatibility) (int64, error) {
	if pool == nil {
		return 0, errors.New("database pool is required")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire compatibility connection: %w", err)
	}
	defer conn.Release()
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return 0, fmt.Errorf("check migration table: %w", err)
	}
	if !exists {
		return 0, errors.New("migration_compatibility: schema_migrations is missing")
	}
	rows, err := conn.Query(ctx, "SELECT version, name, sha256 FROM schema_migrations ORDER BY version")
	if err != nil {
		return 0, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()
	var applied []MigrationDigest
	for rows.Next() {
		var item MigrationDigest
		if err := rows.Scan(&item.Version, &item.Name, &item.SHA256); err != nil {
			return 0, fmt.Errorf("scan applied migration: %w", err)
		}
		applied = append(applied, item)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read applied migrations: %w", err)
	}
	if err := ValidateCompatibility(applied, compatibility); err != nil {
		return 0, err
	}
	return applied[len(applied)-1].Version, nil
}
