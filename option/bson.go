package option

import (
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// MarshalBSONValue implements bson.ValueMarshaler.
func (o Option[T]) MarshalBSONValue() (byte, []byte, error) {
	val, exists := o.Get()
	if !exists {
		btype, buf, err := bson.MarshalValue(bson.Null{})
		return byte(btype), buf, err
	}

	btype, buf, err := bson.MarshalValue(val)
	return byte(btype), buf, err
}

// UnmarshalBSONValue implements bson.ValueUnmarshaler.
func (o *Option[T]) UnmarshalBSONValue(bType byte, raw []byte) error {
	switch bson.Type(bType) {
	case bson.TypeNull:
		o.val = nil

	default:
		valPtr := new(T)

		err := bson.UnmarshalValue(bson.Type(bType), raw, &valPtr)
		if err != nil {
			return errors.Wrapf(err, "failed to unmarshal %T", *o)
		}

		// This may not even be possible, but we should still check.
		if isNil(*valPtr) {
			return errors.Wrapf(err, "refuse to unmarshal nil %T value", *o)
		}

		o.val = valPtr
	}

	return nil
}

// IsZero implements bsoncodec.Zeroer.
func (o Option[T]) IsZero() bool {
	return o.IsNone()
}
