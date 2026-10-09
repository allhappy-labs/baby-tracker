package protocol

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const NormalizationVersion = "nfc-trim-fold-nfc-v1"

var decimalPattern = regexp.MustCompile(`^([+-]?)([0-9]+)(?:\.([0-9]*))?(?:[eE]([+-]?[0-9]+))?$`)

// Decimal is a finite base-ten value rounded to the protocol's six-place
// precision. Its representation never passes through binary floating point.
type Decimal struct {
	coefficient big.Int
	scale       int32
}

func ParseDecimal(raw string) (*Decimal, error) {
	if len(raw) == 0 || len(raw) > 128 {
		return nil, ErrInvalidDecimal
	}
	match := decimalPattern.FindStringSubmatch(raw)
	if match == nil {
		return nil, ErrInvalidDecimal
	}
	exponent := int64(0)
	if match[4] != "" {
		if _, ok := new(big.Int).SetString(match[4], 10); !ok {
			return nil, ErrInvalidDecimal
		}
		parsed := new(big.Int)
		parsed.SetString(match[4], 10)
		if !parsed.IsInt64() {
			return nil, ErrInvalidDecimal
		}
		exponent = parsed.Int64()
		if exponent < -128 || exponent > 128 {
			return nil, ErrInvalidDecimal
		}
	}
	digits := strings.TrimLeft(match[2]+match[3], "0")
	if digits == "" {
		digits = "0"
	}
	coefficient := new(big.Int)
	coefficient.SetString(digits, 10)
	if match[1] == "-" {
		coefficient.Neg(coefficient)
	}
	scale64 := int64(len(match[3])) - exponent
	if scale64 < 0 {
		coefficient.Mul(coefficient, pow10(-scale64))
		scale64 = 0
	}
	if scale64 > 6 {
		drop := scale64 - 6
		divisor := pow10(drop)
		absolute := new(big.Int).Abs(new(big.Int).Set(coefficient))
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(absolute, divisor, remainder)
		twice := new(big.Int).Lsh(remainder, 1)
		comparison := twice.Cmp(divisor)
		if comparison > 0 || (comparison == 0 && quotient.Bit(0) == 1) {
			quotient.Add(quotient, big.NewInt(1))
		}
		if coefficient.Sign() < 0 {
			quotient.Neg(quotient)
		}
		coefficient = quotient
		scale64 = 6
	}
	for scale64 > 0 {
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(coefficient, big.NewInt(10), remainder)
		if remainder.Sign() != 0 {
			break
		}
		coefficient = quotient
		scale64--
	}
	if coefficient.Sign() == 0 {
		coefficient.SetInt64(0)
		scale64 = 0
	}
	return &Decimal{coefficient: *coefficient, scale: int32(scale64)}, nil
}

func MustParseDecimal(raw string) *Decimal {
	decimal, err := ParseDecimal(raw)
	if err != nil {
		panic(err)
	}
	return decimal
}

func pow10(exponent int64) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(exponent), nil)
}

func (decimal Decimal) String() string {
	coefficient := decimal.coefficient.String()
	if decimal.scale == 0 {
		return coefficient
	}
	negative := strings.HasPrefix(coefficient, "-")
	if negative {
		coefficient = coefficient[1:]
	}
	for len(coefficient) <= int(decimal.scale) {
		coefficient = "0" + coefficient
	}
	point := len(coefficient) - int(decimal.scale)
	result := coefficient[:point] + "." + coefficient[point:]
	if negative {
		result = "-" + result
	}
	return result
}

func (decimal Decimal) MarshalJSON() ([]byte, error) { return []byte(decimal.String()), nil }

func (decimal *Decimal) UnmarshalJSON(raw []byte) error {
	if decimal == nil || len(raw) == 0 || raw[0] == '"' || string(raw) == "null" {
		return ErrInvalidDecimal
	}
	parsed, err := ParseDecimal(string(raw))
	if err != nil {
		return err
	}
	*decimal = *parsed
	return nil
}

func (decimal Decimal) CmpInteger(value int64) int {
	right := new(big.Int).Mul(big.NewInt(value), pow10(int64(decimal.scale)))
	return decimal.coefficient.Cmp(right)
}

func NormalizeBabyName(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", ErrInvalidText
	}
	value = norm.NFC.String(value)
	value = strings.TrimFunc(value, isUnicode15WhiteSpace)
	value = norm.NFC.String(cases.Fold().String(value))
	if value == "" {
		return "", ErrInvalidText
	}
	return value, nil
}

func NormalizeDisplayName(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", ErrInvalidText
	}
	value = norm.NFC.String(value)
	value = strings.TrimFunc(value, isUnicode15WhiteSpace)
	if value == "" {
		return "", ErrInvalidText
	}
	return value, nil
}

func NormalizeText(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", ErrInvalidText
	}
	return norm.NFC.String(value), nil
}

func isUnicode15WhiteSpace(character rune) bool {
	return character >= 0x0009 && character <= 0x000d || character == 0x0020 ||
		character == 0x0085 || character == 0x00a0 || character == 0x1680 ||
		character >= 0x2000 && character <= 0x200a || character == 0x2028 ||
		character == 0x2029 || character == 0x202f || character == 0x205f ||
		character == 0x3000
}

func NormalizeTimestamp(value string) (string, error) {
	normalized, parsed, err := normalizeTimestampLexical(value)
	if err != nil {
		return "", err
	}
	if parsed.Year() < 1900 || parsed.Year() > 2100 {
		return "", ErrInvalidTimestamp
	}
	return normalized, nil
}

func normalizeTimestampLexical(value string) (string, time.Time, error) {
	if !utf8.ValidString(value) || !rfc3339MicroInputPattern.MatchString(value) {
		return "", time.Time{}, ErrInvalidTimestamp
	}
	if len(value) >= 6 {
		offset := value[len(value)-6:]
		if (offset[0] == '+' || offset[0] == '-') && offset[1:3] == "14" && offset[4:6] != "00" {
			return "", time.Time{}, ErrInvalidTimestamp
		}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", time.Time{}, ErrInvalidTimestamp
	}
	parsed = parsed.UTC()
	nanoseconds := parsed.Nanosecond()
	microseconds, remainder := nanoseconds/1000, nanoseconds%1000
	if remainder > 500 || (remainder == 500 && microseconds%2 == 1) {
		microseconds++
	}
	parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), 0, time.UTC)
	parsed = parsed.Add(time.Duration(microseconds) * time.Microsecond)
	return parsed.Format("2006-01-02T15:04:05.000000Z"), parsed, nil
}

var rfc3339MicroInputPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-](?:0[0-9]|1[0-4]):[0-5][0-9])$`)

func normalizeTimestampForHash(value string) string {
	if normalized, _, err := normalizeTimestampLexical(value); err == nil {
		return normalized
	}
	normalized, err := NormalizeText(value)
	if err != nil {
		return value
	}
	return normalized
}

func marshalCanonicalJSON(value any) ([]byte, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("canonical JSON: %w", err)
	}
	result := strings.TrimSuffix(buffer.String(), "\n")
	return unescapeJSONLineSeparators([]byte(result)), nil
}

// encoding/json escapes U+2028 and U+2029 for JavaScript compatibility. The
// protocol requires their UTF-8 bytes, but an identical byte sequence inside a
// JSON-escaped backslash ("\\\\u2028") must remain ordinary user text.
func unescapeJSONLineSeparators(encoded []byte) []byte {
	result := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); {
		if encoded[index] != '\\' {
			result = append(result, encoded[index])
			index++
			continue
		}
		start := index
		for index < len(encoded) && encoded[index] == '\\' {
			index++
		}
		run := index - start
		if run%2 == 1 && index+5 <= len(encoded) && (string(encoded[index:index+5]) == "u2028" || string(encoded[index:index+5]) == "u2029") {
			result = append(result, encoded[start:start+run-1]...)
			if encoded[index+4] == '8' {
				result = append(result, []byte("\u2028")...)
			} else {
				result = append(result, []byte("\u2029")...)
			}
			index += 5
			continue
		}
		result = append(result, encoded[start:index]...)
	}
	return result
}
