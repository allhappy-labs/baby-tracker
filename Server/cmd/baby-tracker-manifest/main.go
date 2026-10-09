package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"babytracker/server/internal/migrate"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) > 0 && arguments[0] == "bounds" {
		return runBounds(arguments[1:], output)
	}
	if len(arguments) > 0 && arguments[0] == "verify" {
		return runVerify(arguments[1:], output)
	}
	if len(arguments) > 0 && arguments[0] == "history" {
		return runHistory(arguments[1:], output)
	}
	flags := flag.NewFlagSet("baby-tracker-manifest", flag.ContinueOnError)
	flags.SetOutput(output)
	migrationDirectory := flags.String("migrations", "", "migration SQL directory")
	previousPath := flags.String("previous", "", "previous compatibility manifest")
	outputPath := flags.String("output", "", "output compatibility manifest")
	minimum := flags.Int64("min", 0, "minimum compatible migration version")
	maximum := flags.Int64("max", 0, "maximum compatible migration version")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *migrationDirectory == "" || *previousPath == "" || *outputPath == "" {
		return errors.New("--migrations, --previous, --output, --min, and --max are required")
	}
	previous, err := readManifest(*previousPath)
	if err != nil {
		return err
	}
	next, err := migrate.GenerateManifest(os.DirFS(*migrationDirectory), *minimum, *maximum)
	if err != nil {
		return err
	}
	if err := migrate.VerifyManifestExtension(previous, next); err != nil {
		return err
	}
	return writeManifest(*outputPath, next)
}

func runHistory(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("baby-tracker-manifest history", flag.ContinueOnError)
	flags.SetOutput(output)
	previousPath := flags.String("previous", "", "previous compatibility manifest")
	nextPath := flags.String("next", "", "next compatibility manifest")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *previousPath == "" || *nextPath == "" {
		return errors.New("history requires --previous and --next")
	}
	previous, err := readManifest(*previousPath)
	if err != nil {
		return err
	}
	next, err := readManifest(*nextPath)
	if err != nil {
		return err
	}
	return migrate.VerifyManifestHistory(previous, next)
}

func runBounds(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("baby-tracker-manifest bounds", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("manifest", "", "compatibility manifest")
	minimum := flags.Int64("min", 0, "expected minimum")
	maximum := flags.Int64("max", 0, "expected maximum")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" || *minimum == 0 || *maximum == 0 {
		return errors.New("bounds requires --manifest, --min, and --max")
	}
	manifest, err := readManifest(*path)
	if err != nil {
		return err
	}
	if err := migrate.VerifyManifestExtension(manifest, manifest); err != nil {
		return err
	}
	if manifest.Min != *minimum || manifest.Max != *maximum {
		return fmt.Errorf("manifest bounds [%d,%d] do not match [%d,%d]", manifest.Min, manifest.Max, *minimum, *maximum)
	}
	return nil
}

func runVerify(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("baby-tracker-manifest verify", flag.ContinueOnError)
	flags.SetOutput(output)
	previousPath := flags.String("previous", "", "previous compatibility manifest")
	nextPath := flags.String("next", "", "next compatibility manifest")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *previousPath == "" || *nextPath == "" {
		return errors.New("verify requires --previous and --next")
	}
	previous, err := readManifest(*previousPath)
	if err != nil {
		return err
	}
	next, err := readManifest(*nextPath)
	if err != nil {
		return err
	}
	return migrate.VerifyManifestExtension(previous, next)
}

func readManifest(path string) (migrate.Compatibility, error) {
	file, err := os.Open(path)
	if err != nil {
		return migrate.Compatibility{}, fmt.Errorf("open previous manifest: %w", err)
	}
	defer file.Close()
	var compatibility migrate.Compatibility
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&compatibility); err != nil {
		return migrate.Compatibility{}, fmt.Errorf("decode previous manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return migrate.Compatibility{}, errors.New("previous manifest has trailing data")
	}
	return compatibility, nil
}

func writeManifest(path string, compatibility migrate.Compatibility) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".compatibility-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(compatibility); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	remove = false
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
