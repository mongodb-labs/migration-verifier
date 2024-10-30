package verifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
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

func (ns *Namespace) String() string {
	return fmt.Sprintf("{ db: %s, coll: %s }", ns.DB, ns.Coll)
}

func (ns *Namespace) FullName() string {
	return ns.DB + "." + ns.Coll
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
	ID            interface{} `bson:"id"`
	SrcNamespace  string      `bson:"srcNamespace"`
	DestNamespace string      `bson:"destNamespace"`
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

// GetLastOpTimeAndSyncShardClusterTime retrieves the last operation time on the source.
func GetLastOpTimeAndSyncShardClusterTime(
	ctx context.Context,
	logger *logger.Logger,
	retryer retry.Retryer,
	client *mongo.Client,
	retryOnLockFailed bool,
) (primitive.Timestamp, error) {
	// 'appendOplogNote' will perform a no-op write on the source cluster. When run against
	// sharded clusters, this command will perform the no-op write on all shards. In receiving
	// the shard responses, the driver notes the highest $clusterTime amongst all of them.
	var response *mongo.SingleResult
	appendOplogNoteCmd := bson.D{
		{"appendOplogNote", 1},
		{"data", bson.D{{"migration-verifier", "last op fetching"}}},
	}

	if retryOnLockFailed {
		retryer = retryer.WithErrorCodes(util.LockFailed)
	}
	err := retryer.RunForTransientErrorsOnly(ctx, logger, func(ri *retry.Info) error {
		ri.Log(logger.Logger,
			"appendOplogNote",
			"source",
			"",
			"",
			fmt.Sprintf("Running appendOplogNote command. %v", appendOplogNoteCmd))
		response = client.Database("admin").RunCommand(ctx, appendOplogNoteCmd)
		if err := response.Err(); err != nil {
			return err
		}

		return nil
	})

	// Get the `operationTime` from the response and return it.
	respStruct := struct {
		OperationTime primitive.Timestamp `bson:"operationTime"`
	}{}

	err = response.Decode(&respStruct)
	if err != nil {
		return primitive.Timestamp{}, errors.Wrap(err,
			"failed to parse source cluster's appendOplogNote response")
	}

	return respStruct.OperationTime, nil
}
