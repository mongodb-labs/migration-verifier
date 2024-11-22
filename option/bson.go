package option

import (
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// MarshalBSONValue implements bson.ValueMarshaler.
func (o Option[T]) MarshalBSONValue() (bsontype.Type, []byte, error) {
	val, exists := o.Get()
	if !exists {
		return bson.MarshalValue(primitive.Null{})
	}

	return bson.MarshalValue(val)
}

// UnmarshalBSONValue implements bson.ValueUnmarshaler.
func (o *Option[T]) UnmarshalBSONValue(bType bsontype.Type, raw []byte) error {

	switch bType {
	case bson.TypeNull:
		o.val = nil

	default:
		valPtr := new(T)

		err := bson.UnmarshalValue(bType, raw, &valPtr)
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
