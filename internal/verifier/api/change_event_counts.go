package api

import (
	"github.com/10gen/migration-verifier/mslices"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
)

// ChangeEventCounts tallies cumulative change events seen by a change reader,
// across all generations, since the verifier first started.
type ChangeEventCounts struct {
	// NB:  These are int64 because they get serialized to extJSON.
	// The API implementation’s Go types are unfortunately inconsistent.
	Insert  int64
	Update  int64
	Replace int64
	Delete  int64

	Create                   int64 `bson:"create"`
	Modify                   int64 `bson:"modify"`
	CreateIndexes            int64 `bson:"createIndexes"`
	DropIndexes              int64 `bson:"dropIndexes"`
	ShardCollection          int64 `bson:"shardCollection"`
	ReshardCollection        int64 `bson:"reshardCollection"`
	RefineCollectionShardKey int64 `bson:"refineCollectionShardKey"`
}

func (cec ChangeEventCounts) CountDDL() int64 {
	return lo.Sum(mslices.Of(
		cec.Create,
		cec.Modify,
		cec.CreateIndexes,
		cec.DropIndexes,
		cec.ShardCollection,
		cec.ReshardCollection,
		cec.RefineCollectionShardKey,
	))
}

var _ zerolog.LogObjectMarshaler = ChangeEventCounts{}

func (cec ChangeEventCounts) Total() int64 {
	return cec.Insert + cec.Update + cec.Replace + cec.Delete + cec.CountDDL()
}

func (cec ChangeEventCounts) MarshalZerologObject(e *zerolog.Event) {
	e.
		Int64("insert", cec.Insert).
		Int64("update", cec.Update).
		Int64("replace", cec.Replace).
		Int64("delete", cec.Delete).
		Int64("create", cec.Create).
		Int64("modify", cec.Modify).
		Int64("createIndexes", cec.CreateIndexes).
		Int64("dropIndexes", cec.DropIndexes).
		Int64("shardCollection", cec.ShardCollection).
		Int64("reshardCollection", cec.ReshardCollection).
		Int64("refineCollectionShardKey", cec.RefineCollectionShardKey)
}
