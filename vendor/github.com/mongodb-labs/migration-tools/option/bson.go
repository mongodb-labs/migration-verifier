package option

import (
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var _ bson.ValueMarshaler = Option[int]{}
var _ bson.ValueUnmarshaler = &Option[int]{}
var _ bson.Zeroer = Option[int]{}

// MarshalBSONValue implements bson.ValueMarshaler.
func (o Option[T]) MarshalBSONValue() (byte, []byte, error) {

	var bType bson.Type
	var data []byte
	var err error

	if val, exists := o.Get(); exists {
		bType, data, err = bson.MarshalValue(val)

		return byte(bType), data, err
	}

	return byte(bson.TypeNull), nil, nil
}

// UnmarshalBSONValue implements bson.ValueUnmarshaler.
func (o *Option[T]) UnmarshalBSONValue(bType byte, raw []byte) error {
	switch bson.Type(bType) {
	case bson.TypeNull:
		o.isSet = false

	default:
		valPtr := new(T)

		err := bson.UnmarshalValue(bson.Type(bType), raw, &valPtr)
		if err != nil {
			return fmt.Errorf("unmarshal %T: %w", *o, err)
		}

		// This may not even be possible, but we should still check.
		if isNil(*valPtr) {
			return fmt.Errorf("refuse to unmarshal nil %T value", *o)
		}

		o.val = *valPtr
		o.isSet = true
	}

	return nil
}

// IsZero implements bson.Zeroer.
func (o Option[T]) IsZero() bool {
	return o.IsNone()
}
