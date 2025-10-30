package util

import (
	"fmt"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

const (
	uuidBinarySubtype = 4
)

// UUID is a type definition that implements the ValueMarshler/ValueUnmarshaler and
// KeyMarshaler/KeyUnmarshaler interfaces. This allows us to easily convert between UUID
// and BSON, both when UUID is used as a field in a document and when UUID is used as the
// key in a map.
type UUID uuid.UUID

var (
	_ bson.ValueMarshaler   = UUID{}
	_ bson.ValueUnmarshaler = (*UUID)(nil)
)

// NewUUID constructs a new, randomly-generated UUID.
func NewUUID() UUID {
	return UUID(uuid.New())
}

// MarshalBSONValue is used to marshal UUID objects into BSON. This implements the
// ValueMarshaler interface.
func (u UUID) MarshalBSONValue() (bson.Type, []byte, error) {
	val := bsoncore.AppendBinary(nil, uuidBinarySubtype, u[:])
	return bson.TypeBinary, val, nil
}

// UnmarshalBSONValue is used to unmarshal BSON into UUID objects. This implements the
// ValueUnmarshaler interface.
func (u *UUID) UnmarshalBSONValue(bsonType bson.Type, data []byte) error {
	if bsonType != bson.TypeBinary {
		return fmt.Errorf("cannot decoded BSON value of type %s as a UUID", bsonType)
	}

	subtype, binData, rem, ok := bsoncore.ReadBinary(data)
	if !ok || len(rem) != 0 {
		return fmt.Errorf("failed to parse BSON value %v as BSON binary", data)
	}
	if subtype != uuidBinarySubtype {
		return fmt.Errorf("expected BSON value %v to have subtype %d, got %d", data, uuidBinarySubtype, subtype)
	}
	copy((*u)[:], binData)
	return nil
}

func (u UUID) String() string {
	return uuid.UUID(u).String()
}

// ParseBinary returns a UUID from its binary format.
func ParseBinary(binUUID *bson.Binary) UUID {
	return UUID(uuid.Must(uuid.FromBytes(binUUID.Data)))
}
