package types

import "encoding/json"

// MixedValue is a raw JSON value whose underlying shape (string, object,
// array, number, boolean) is decided at use time. Hyperliquid returns
// several heterogeneous fields (notably order statuses) where each
// element can be either a tagged object or a bare string token.
type MixedValue json.RawMessage

// UnmarshalJSON stores the incoming JSON bytes verbatim so callers can
// inspect the underlying shape later via Type, Parse, Object, Array or
// String.
func (mv *MixedValue) UnmarshalJSON(data []byte) error {
	*mv = data
	return nil
}

// MarshalJSON returns the stored JSON bytes verbatim.
func (mv MixedValue) MarshalJSON() ([]byte, error) {
	return mv, nil
}

// String decodes the value as a JSON string. The second return value is
// false when the stored bytes are not a valid JSON string.
func (mv *MixedValue) String() (string, bool) {
	var s string
	if err := json.Unmarshal(*mv, &s); err != nil {
		return "", false
	}
	return s, true
}

// Object decodes the value as a JSON object. The second return value is
// false when the stored bytes are not a valid JSON object.
func (mv *MixedValue) Object() (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal(*mv, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

// Array decodes the value as a JSON array of raw elements. The second
// return value is false when the stored bytes are not a valid JSON array.
func (mv *MixedValue) Array() ([]json.RawMessage, bool) {
	var arr []json.RawMessage
	if err := json.Unmarshal(*mv, &arr); err != nil {
		return nil, false
	}
	return arr, true
}

// Parse unmarshals the stored bytes into v, the same way encoding/json
// would. Use it once Type has confirmed the underlying shape.
func (mv *MixedValue) Parse(v any) error {
	return json.Unmarshal(*mv, v)
}

// Type reports the JSON kind of the stored value. It returns one of
// "string", "object", "array", "boolean", "number", or "null", inferred
// from the first non-whitespace byte without a full decode.
func (mv *MixedValue) Type() string {
	if mv == nil || len(*mv) == 0 {
		return "null"
	}

	first := (*mv)[0]

	switch first {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// MixedArray is a slice of MixedValue. It exists so that wire payloads
// whose elements have heterogeneous shapes can be decoded without
// committing to a single Go type up front.
type MixedArray []MixedValue

// UnmarshalJSON decodes data into a slice of MixedValue entries,
// preserving each element's raw JSON for later inspection.
func (ma *MixedArray) UnmarshalJSON(data []byte) error {
	var rawArr []MixedValue
	if err := json.Unmarshal(data, &rawArr); err != nil {
		return err
	}

	*ma = rawArr
	return nil
}
