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
	if cec.Insert > 0 {
		e.Int64("insert", cec.Insert)
	}
	if cec.Update > 0 {
		e.Int64("update", cec.Update)
	}
	if cec.Replace > 0 {
		e.Int64("replace", cec.Replace)
	}
	if cec.Delete > 0 {
		e.Int64("delete", cec.Delete)
	}
	if cec.Create > 0 {
		e.Int64("create", cec.Create)
	}
	if cec.Modify > 0 {
		e.Int64("modify", cec.Modify)
	}
	if cec.CreateIndexes > 0 {
		e.Int64("createIndexes", cec.CreateIndexes)
	}
	if cec.DropIndexes > 0 {
		e.Int64("dropIndexes", cec.DropIndexes)
	}
	if cec.ShardCollection > 0 {
		e.Int64("shardCollection", cec.ShardCollection)
	}
	if cec.ReshardCollection > 0 {
		e.Int64("reshardCollection", cec.ReshardCollection)
	}
	if cec.RefineCollectionShardKey > 0 {
		e.Int64("refineCollectionShardKey", cec.RefineCollectionShardKey)
	}
}
