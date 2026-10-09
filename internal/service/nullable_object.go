package service

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// NullableObject is a JSON object field that the API documents as nullable.
// encoding/json/v2 writes a nil map as {}, and the `format:emitnull` tag option
// does not apply to map[string]any, so the type writes nil as null itself.
type NullableObject map[string]any

// MarshalJSONTo writes null for a nil object and the object otherwise.
func (o NullableObject) MarshalJSONTo(enc *jsontext.Encoder) error {
	if o == nil {
		return enc.WriteToken(jsontext.Null)
	}
	return json.MarshalEncode(enc, map[string]any(o))
}
