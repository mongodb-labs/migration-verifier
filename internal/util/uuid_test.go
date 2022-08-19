package util

import (
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
)

func (suite *UnitTestSuite) TestUUID_MarshalAndUnmarshal() {
	key1, val1, key2, val2 := NewUUID(), NewUUID(), NewUUID(), NewUUID()

	// Test marshalling and unmarshalling a map of UUID keys to UUID values.
	uuidMap := make(map[UUID]UUID, 2)
	uuidMap[key1] = val1
	uuidMap[key2] = val2
	bytes, err := bson.Marshal(uuidMap)
	assert.Equal(suite.T(), nil, err)

	retrievedUUIDMap := make(map[UUID]UUID, 2)
	err = bson.Unmarshal(bytes, retrievedUUIDMap)
	assert.Equal(suite.T(), nil, err)

	assert.Equal(suite.T(), val1, retrievedUUIDMap[key1])
	assert.Equal(suite.T(), val2, retrievedUUIDMap[key2])
}
