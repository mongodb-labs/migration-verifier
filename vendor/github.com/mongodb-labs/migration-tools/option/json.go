package option

import (
	"bytes"
	"encoding/json"
)

var _ json.Marshaler = Option[int]{}
var _ json.Unmarshaler = &Option[int]{}

// MarshalJSON encodes Option into json.
func (o Option[T]) MarshalJSON() ([]byte, error) {
	val, exists := o.Get()
	if exists {
		return json.Marshal(val)
	}

	return json.Marshal(nil)
}

// UnmarshalJSON decodes Option from json.
func (o *Option[T]) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		o.isSet = false
	} else {
		val := *new(T)

		// Reject unrecognized keys, as callers that decode into an Option have
		// no other way to ask for that: encoding/json's own setting does not
		// reach inside a custom unmarshaler.
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()

		err := dec.Decode(&val)
		if err != nil {
			return err
		}

		o.val = val
		o.isSet = true
	}

	return nil
}
