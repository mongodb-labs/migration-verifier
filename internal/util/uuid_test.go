package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type UnitTestSuite struct {
	suite.Suite
}

func TestUnitTestSuite(t *testing.T) {
	ts := new(UnitTestSuite)
	suite.Run(t, ts)
}

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
