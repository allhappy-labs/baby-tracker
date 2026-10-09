package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
)

type ExportConfig struct {
	Enabled           bool
	WorkerDatabaseURL string
	ActiveKeyID       string
	Keys              map[string][32]byte
}

var exportKeyIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

// LoadExport keeps the worker's database credential and encryption keys out of
// the runtime configuration. An absent group disables exports; partial groups
// fail closed so the API never claims an export that cannot be generated.
func LoadExport(getenv func(string) string) (ExportConfig, error) {
	if getenv == nil {
		return ExportConfig{}, errors.New("configuration getter is required")
	}
	const databaseFile = "BT_EXPORT_DATABASE_URL_FILE"
	const keysFile = "BT_EXPORT_KEYS_FILE"
	const keyID = "BT_EXPORT_KEY_ID"
	if getenv(databaseFile) == "" && getenv(keysFile) == "" && getenv(keyID) == "" && getenv("BT_EXPORT_DATABASE_URL") == "" {
		return ExportConfig{}, nil
	}
	if getenv(databaseFile) == "" || getenv(keysFile) == "" || !exportKeyIDPattern.MatchString(getenv(keyID)) {
		return ExportConfig{}, errors.New("export requires database URL file, key file, and valid key ID")
	}
	databaseURL, err := credentialURL(getenv, "BT_EXPORT_DATABASE_URL", databaseFile)
	if err != nil || getenv("BT_EXPORT_DATABASE_URL") != "" {
		return ExportConfig{}, errors.New("BT_EXPORT_DATABASE_URL_FILE is required")
	}
	info, err := os.Lstat(getenv(keysFile))
	if err != nil || !exactOperatorSecretMode(info.Mode()) {
		return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE must be a regular nonsymlink file with mode 0600")
	}
	raw, err := readRegularFile(getenv(keysFile), 16<<10)
	if err != nil {
		return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE could not be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE must be a JSON object")
	}
	keys := make(map[string][32]byte)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE is invalid")
		}
		id, ok := token.(string)
		if !ok || !exportKeyIDPattern.MatchString(id) || len(keys) >= 8 {
			return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE has invalid key IDs")
		}
		if _, duplicate := keys[id]; duplicate {
			return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE has duplicate key IDs")
		}
		var encoded string
		if err := decoder.Decode(&encoded); err != nil || len(encoded) != 64 {
			return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE has invalid key material")
		}
		decoded, err := hex.DecodeString(encoded)
		if err != nil || hex.EncodeToString(decoded) != encoded {
			return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE has invalid key material")
		}
		var key [32]byte
		copy(key[:], decoded)
		if key == ([32]byte{}) {
			return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE has invalid key material")
		}
		for _, existing := range keys {
			if existing == key {
				return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE has repeated key material")
			}
		}
		keys[id] = key
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.More() {
		return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ExportConfig{}, errors.New("BT_EXPORT_KEYS_FILE has trailing data")
	}
	if _, ok := keys[getenv(keyID)]; !ok {
		return ExportConfig{}, errors.New("BT_EXPORT_KEY_ID is absent from BT_EXPORT_KEYS_FILE")
	}
	return ExportConfig{Enabled: true, WorkerDatabaseURL: databaseURL, ActiveKeyID: getenv(keyID), Keys: keys}, nil
}
