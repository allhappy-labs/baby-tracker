package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

var capabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

var (
	errInvalidRequest        = protocolError("invalid_request")
	errUnsupportedProtocol   = protocolError("unsupported_protocol")
	errUnsupportedCapability = protocolError("unsupported_capability")
)

type protocolError string

func (e protocolError) Error() string { return string(e) }

func ErrorCodeOf(err error) string {
	var typed protocolError
	if errors.As(err, &typed) {
		return string(typed)
	}
	return ""
}

func ValidateCapabilities(requested, supported []string) error {
	if requested == nil || len(requested) > 16 {
		return errUnsupportedCapability
	}
	seen := make(map[string]struct{}, len(requested))
	for _, capability := range requested {
		if len(capability) == 0 || len(capability) > 64 || !capabilityPattern.MatchString(capability) ||
			!slices.Contains(supported, capability) {
			return errUnsupportedCapability
		}
		if _, exists := seen[capability]; exists {
			return errUnsupportedCapability
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func DecodeRequest(reader io.Reader, maximum int64, target any) error {
	if reader == nil || target == nil || maximum <= 0 {
		return errInvalidRequest
	}
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil || len(data) == 0 || int64(len(data)) > maximum || !utf8.Valid(data) {
		return errInvalidRequest
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errInvalidRequest
	}
	if err := rejectDuplicateRequestKeys(data); err != nil {
		return errInvalidRequest
	}
	if err := rejectUnknownRequestKeys(data, reflect.TypeOf(target)); err != nil {
		return errInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errInvalidRequest
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errInvalidRequest
	}
	request, ok := target.(interface{ requestMetadata() RequestMeta })
	if !ok {
		return errInvalidRequest
	}
	if err := validateRequestMeta(request.requestMetadata()); err != nil {
		return err
	}
	return nil
}

func rejectUnknownRequestKeys(data []byte, targetType reflect.Type) error {
	if targetType == nil || targetType.Kind() != reflect.Pointer || targetType.Elem().Kind() != reflect.Struct {
		return errInvalidRequest
	}
	return validateExactJSONObject(data, targetType.Elem())
}

func validateExactJSONObject(data []byte, valueType reflect.Type) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errInvalidRequest
	}
	fields := make(map[string]reflect.Type)
	collectJSONFields(valueType, fields)
	for name, raw := range object {
		fieldType, ok := fields[name]
		if !ok {
			return errInvalidRequest
		}
		if err := validateNestedRequestValue(raw, fieldType); err != nil {
			return err
		}
	}
	return nil
}

func collectJSONFields(valueType reflect.Type, fields map[string]reflect.Type) {
	for index := 0; index < valueType.NumField(); index++ {
		field := valueType.Field(index)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				collectJSONFields(embedded, fields)
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
}

func validateNestedRequestValue(raw json.RawMessage, valueType reflect.Type) error {
	unmarshalerType := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	for valueType.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		if valueType.Implements(unmarshalerType) {
			return nil
		}
		valueType = valueType.Elem()
	}
	if valueType.Implements(unmarshalerType) || reflect.PointerTo(valueType).Implements(unmarshalerType) {
		return nil
	}
	switch valueType.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		text := string(bytes.TrimSpace(raw))
		if !canonicalIntegerPattern.MatchString(text) {
			return errInvalidRequest
		}
		value, err := strconv.ParseUint(text, 10, 64)
		if err != nil || value > MaxSafeInteger || valueType.OverflowUint(value) {
			return errInvalidRequest
		}
		return nil
	case reflect.Struct:
		return validateExactJSONObject(raw, valueType)
	case reflect.Slice, reflect.Array:
		elementType := valueType.Elem()
		for elementType.Kind() == reflect.Pointer {
			elementType = elementType.Elem()
		}
		if elementType.Kind() != reflect.Struct {
			return nil
		}
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return errInvalidRequest
		}
		for _, element := range elements {
			if err := validateExactJSONObject(element, elementType); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectDuplicateRequestKeys(data []byte) error {
	if !validJSONStringEscapes(data) {
		return errInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 16 {
			return errInvalidRequest
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errInvalidRequest
				}
				if _, exists := seen[key]; exists {
					return errInvalidRequest
				}
				seen[key] = struct{}{}
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		default:
			return errInvalidRequest
		}
	}
	if err := walk(1); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errInvalidRequest
	}
	return nil
}

func validJSONStringEscapes(data []byte) bool {
	inside := false
	for index := 0; index < len(data); index++ {
		character := data[index]
		if !inside {
			if character == '"' {
				inside = true
			}
			continue
		}
		if character == '"' {
			inside = false
			continue
		}
		if character < 0x20 {
			return false
		}
		if character != '\\' {
			continue
		}
		index++
		if index >= len(data) {
			return false
		}
		escape := data[index]
		if strings.ContainsRune(`"\\/bfnrt`, rune(escape)) {
			continue
		}
		if escape != 'u' || index+4 >= len(data) {
			return false
		}
		code, ok := parseHex16(data[index+1 : index+5])
		if !ok {
			return false
		}
		index += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if index+6 >= len(data) || data[index+1] != '\\' || data[index+2] != 'u' {
				return false
			}
			low, ok := parseHex16(data[index+3 : index+7])
			if !ok || low < 0xdc00 || low > 0xdfff {
				return false
			}
			index += 6
		}
	}
	return !inside
}

func parseHex16(raw []byte) (uint16, bool) {
	if len(raw) != 4 {
		return 0, false
	}
	var value uint16
	for _, character := range raw {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value += uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value += uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value += uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}
