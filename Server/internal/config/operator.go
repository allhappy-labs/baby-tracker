package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
)

const operatorSecretLimit = 256

var operatorPolicyIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)

type OperatorConfig struct {
	OperatorID          string
	OwnershipPolicyID   string
	EntitlementPolicyID string
	ConfirmationKey     [32]byte
}

// LoadOperator loads the separately mounted operator policy and confirmation
// material. It deliberately does not accept direct-value secret fallbacks.
// An empty environment disables both dangerous command families.
func LoadOperator(getenv func(string) string) (OperatorConfig, error) {
	if getenv == nil {
		return OperatorConfig{}, errors.New("configuration getter is required")
	}
	for _, name := range []string{"BT_OPERATOR_OWNERSHIP_POLICY_ID", "BT_OPERATOR_ENTITLEMENT_POLICY_ID", "BT_OPERATOR_CONFIRMATION_KEY"} {
		if getenv(name) != "" {
			return OperatorConfig{}, fmt.Errorf("%s is forbidden; use the corresponding file setting", name)
		}
	}
	operatorID := getenv("BT_OPERATOR_ID")
	ownershipPath := getenv("BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE")
	entitlementPath := getenv("BT_OPERATOR_ENTITLEMENT_POLICY_ID_FILE")
	keyPath := getenv("BT_OPERATOR_CONFIRMATION_KEY_FILE")
	if operatorID == "" && ownershipPath == "" && entitlementPath == "" && keyPath == "" {
		return OperatorConfig{}, nil
	}
	if ownershipPath == "" && entitlementPath == "" {
		return OperatorConfig{}, errors.New("an operator policy file is required when operator configuration is present")
	}
	if !uuidPattern.MatchString(operatorID) || operatorID == "00000000-0000-0000-0000-000000000000" {
		return OperatorConfig{}, errors.New("BT_OPERATOR_ID must be a canonical nonzero UUID")
	}
	if keyPath == "" {
		return OperatorConfig{}, errors.New("BT_OPERATOR_CONFIRMATION_KEY_FILE is required when an operator policy is enabled")
	}
	configuration := OperatorConfig{OperatorID: operatorID}
	var err error
	if ownershipPath != "" {
		configuration.OwnershipPolicyID, err = readOperatorPolicyFile("BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE", ownershipPath)
		if err != nil {
			return OperatorConfig{}, err
		}
	}
	if entitlementPath != "" {
		configuration.EntitlementPolicyID, err = readOperatorPolicyFile("BT_OPERATOR_ENTITLEMENT_POLICY_ID_FILE", entitlementPath)
		if err != nil {
			return OperatorConfig{}, err
		}
	}
	keyText, err := readOperatorSecretFile("BT_OPERATOR_CONFIRMATION_KEY_FILE", keyPath)
	if err != nil || len(keyText) != 64 || strings.ToLower(keyText) != keyText {
		return OperatorConfig{}, errors.New("BT_OPERATOR_CONFIRMATION_KEY_FILE must contain 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(keyText)
	if err != nil || len(decoded) != len(configuration.ConfirmationKey) {
		return OperatorConfig{}, errors.New("BT_OPERATOR_CONFIRMATION_KEY_FILE must contain 64 lowercase hexadecimal characters")
	}
	copy(configuration.ConfirmationKey[:], decoded)
	return configuration, nil
}

func readOperatorPolicyFile(name, path string) (string, error) {
	value, err := readOperatorSecretFile(name, path)
	if err != nil {
		return "", err
	}
	if !operatorPolicyIDPattern.MatchString(value) {
		return "", fmt.Errorf("%s must contain a valid operator policy ID", name)
	}
	return value, nil
}

func readOperatorSecretFile(name, path string) (string, error) {
	readError := fmt.Errorf("%s must be a regular nonsymlink file with mode 0600", name)
	before, err := os.Lstat(path)
	if err != nil || !exactOperatorSecretMode(before.Mode()) {
		return "", readError
	}
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", readError
	}
	file := os.NewFile(uintptr(descriptor), name)
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !exactOperatorSecretMode(after.Mode()) || !os.SameFile(before, after) {
		return "", readError
	}
	data, err := io.ReadAll(io.LimitReader(file, operatorSecretLimit+1))
	if err != nil || len(data) > operatorSecretLimit {
		return "", fmt.Errorf("%s has invalid contents", name)
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(after, final) || !exactOperatorSecretMode(final.Mode()) {
		return "", readError
	}
	value := string(data)
	if strings.HasSuffix(value, "\n") {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("%s has invalid contents", name)
	}
	return value, nil
}

func exactOperatorSecretMode(mode os.FileMode) bool {
	const special = os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	return mode.IsRegular() && mode.Perm() == 0o600 && mode&special == 0
}
