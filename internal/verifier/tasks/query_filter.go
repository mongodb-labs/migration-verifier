package tasks

import (
	"slices"

	"github.com/10gen/migration-verifier/internal/partitions"
)

// QueryFilter stores namespace and partition info
type QueryFilter struct {
	Partition *partitions.Partition `bson:"partition"`
	ShardKeys []string
	Namespace string `bson:"namespace"    json:"namespace"`
	To        string `bson:"to,omitempty" json:"to,omitempty"`
}

func (qf QueryFilter) GetDocKeyFields() []string {
	if slices.Contains(qf.ShardKeys, "_id") {
		return slices.Clone(qf.ShardKeys)
	}

	return append(
		[]string{"_id"},
		qf.ShardKeys...,
	)
}
