package verifier

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Specify states
const (
	Unprocessed = "unprocessed"
	Upserted    = "upserted"
	Deleted     = "deleted"
	NotFound    = "not found"
)

// Specify error codes
const (
	ErrorGetClusterStatsSummary = 522601
	ErrorTrackedDirtyBytes
	ErrorReplicationDelayed

	ErrorInsertTask
	ErrorUpdateParentTask
	ErrorUpdateTask
)

// SplitNamespace returns db, collection
func SplitNamespace(namespace string) (string, string) {
	dot := strings.Index(namespace, ".")
	if dot < 0 {
		return namespace, ""
	}
	return namespace[:dot], namespace[dot+1:]
}

// Returns full name of collection including database name
func FullName(collection *mongo.Collection) string {
	return collection.Database().Name() + "." + collection.Name()
}

// Namespace represents a db and coll.
type Namespace struct {
	// The database and collection name of the namespace being copied.
	DB   string `bson:"db"`
	Coll string `bson:"coll"`
}

var _ bson.Unmarshaler = &Namespace{}

func (ns *Namespace) UnmarshalBSON(in []byte) error {
	panic("Use UnmarshalFromBSON instead.")
}

func (ns *Namespace) String() string {
	return fmt.Sprintf("{ db: %s, coll: %s }", ns.DB, ns.Coll)
}

func (ns *Namespace) FullName() string {
	return ns.DB + "." + ns.Coll
}

// UnmarshalBSON implements bson.Unmarshaler. We define this manually to
// avoid reflection, which can substantially impede performance in “hot”
// code paths like this.
func (ns *Namespace) UnmarshalFromBSON(in []byte) error {
	for el, err := range mbson.RawElements(in) {
		if err != nil {
			return errors.Wrap(err, "iterating BSON fields")
		}

		key, err := el.KeyErr()
		if err != nil {
			return errors.Wrap(err, "reading BSON field name")
		}

		switch key {
		case "db":
			if err := mbson.UnmarshalElementValue(el, &ns.DB); err != nil {
				return err
			}
		case "coll":
			if err := mbson.UnmarshalElementValue(el, &ns.Coll); err != nil {
				return err
			}
		}
	}

	return nil
}

// NewNamespace returns a new Namespace struct with the given parameters.
func NewNamespace(db, coll string) *Namespace {
	return &Namespace{
		DB:   db,
		Coll: coll,
	}
}

// Refetch contains the data necessary to track a refretch
type Refetch struct {
	ID            any    `bson:"id"`
	SrcNamespace  string `bson:"srcNamespace"`
	DestNamespace string `bson:"destNamespace"`
	Status        string
}

// TaskError contains error Code and Message
type TaskError struct {
	Code    int
	Message string
}

func (e TaskError) Error() string {
	return e.Message
}

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

func (verifier *Verifier) doInMetaTransaction(
	ctx context.Context,
	todo func(context.Context, context.Context) error,
) error {
	if mongo.SessionFromContext(ctx) != nil {
		verifier.logger.Panic().
			Msg("Context indicates an active session when it should not.")
	}

	session, err := verifier.metaClient.StartSession()
	if err != nil {
		return errors.Wrap(err, "failed to start metadata session")
	}

	defer session.EndSession(ctx)

	_, err = session.WithTransaction(ctx, func(sctx context.Context) (any, error) {
		return nil, todo(ctx, sctx)
	})

	return err
}
