package mbson

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestGetBSONArraySize_RawValue(t *testing.T) {
	vals := []bson.RawValue{
		ToRawValue(123),
		ToRawValue("heyhey"),
	}

	docBytes := lo.Must(bson.Marshal(bson.D{{"foo", vals}}))
	valsBytes := lo.Must(bson.Raw(docBytes).LookupErr("foo")).Value

	assert.Len(t, valsBytes, GetBSONArraySize(vals))
}

func TestGetBSONArraySize_Raw(t *testing.T) {
	docs := []bson.Raw{
		lo.Must(bson.Marshal(bson.D{})),
		lo.Must(bson.Marshal(bson.D{
			{"foo", 123.1},
		})),
		lo.Must(bson.Marshal(bson.D{
			{"foo", bson.D{
				{"bar", 999.1},
			}},
		})),
	}

	docBytes := lo.Must(bson.Marshal(bson.D{{"foo", docs}}))
	docsBytes := lo.Must(bson.Raw(docBytes).LookupErr("foo")).Value

	assert.Len(t, docsBytes, GetBSONArraySize(docs))
}
