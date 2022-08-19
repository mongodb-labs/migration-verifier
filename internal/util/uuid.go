package util

import (
	"fmt"

	"github.com/10gen/mongosync/internal/mongosync/constants"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson/bsoncodec"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
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
	_ bsoncodec.ValueMarshaler   = UUID{}
	_ bsoncodec.ValueUnmarshaler = (*UUID)(nil)
	_ bsoncodec.KeyMarshaler     = UUID{}
	_ bsoncodec.KeyUnmarshaler   = (*UUID)(nil)
)

// NewUUID constructs a new, randomly-generated UUID.
func NewUUID() UUID {
	return UUID(uuid.New())
}

// MarshalBSONValue is used to marshal UUID objects into BSON. This implements the
// ValueMarshaler interface.
func (u UUID) MarshalBSONValue() (bsontype.Type, []byte, error) {
	val := bsoncore.AppendBinary(nil, uuidBinarySubtype, u[:])
	return bsontype.Binary, val, nil
}

// UnmarshalBSONValue is used to unmarshal BSON into UUID objects. This implements the
// ValueUnmarshaler interface.
func (u *UUID) UnmarshalBSONValue(bsonType bsontype.Type, data []byte) error {
	if bsonType != bsontype.Binary {
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

// MarshalKey is used to marshal a map with UUID keys into BSON. This implements the
// KeyMarshaler interface.
func (u UUID) MarshalKey() (string, error) {
	return u.String(), nil
}

// UnmarshalKey is used to unmarshal BSON into a map with UUID keys. This implements the
// KeyUnmarshaler interface.
func (u *UUID) UnmarshalKey(key string) error {
	parsedUUID, err := uuid.Parse(key)
	if err != nil {
		return fmt.Errorf("error parsing string as UUID: %w", err)
	}

	*u = UUID(parsedUUID)
	return nil
}

func (u UUID) String() string {
	return uuid.UUID(u).String()
}

// ParseBinary returns a UUID from its binary format.
func ParseBinary(binUUID *primitive.Binary) UUID {
	return UUID(uuid.Must(uuid.FromBytes(binUUID.Data)))
}

// GetCollTempName returns a temporary collection name used for creating or renaming collections on destination cluster.
func GetCollTempName(collUUID UUID) string {
	return fmt.Sprintf("%s.%s", constants.CollTempNamePrefix, collUUID)
}

// GetCollationTranslationCollName returns the full name of the collation collection given the collection
// name of the change event.
func GetCollationTranslationCollName(dbName string, collUUID *UUID) string {
	return fmt.Sprintf("%s.%s.%s", constants.CollationTranslationCollPrefix, dbName, collUUID)
}
