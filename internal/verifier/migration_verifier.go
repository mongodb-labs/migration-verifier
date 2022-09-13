package verifier

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math/rand"
	_ "net/http/pprof"
	"os"
	"path"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/uuidutil"
	"github.com/olekukonko/tablewriter"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

const (
	//TODO: add comments for each of these so the warnings will stop :)
	Missing           = "Missing"
	Failed            = "Failed"
	Mismatch          = "Mismatch"
	ClusterTarget     = "dstClient"
	ClusterSource     = "srcClient"
	SrcNamespaceField = "query_filter.namespace"
	DstNamespaceField = "query_filter.to"
	NumWorkers        = 10
	refetch           = "TODO_CHANGE_ME_REFETCH"
	Idle              = "idle"
	Check             = "check"
	Recheck           = "recheck"
)

// Verifier is the main state for the migration verifier
type Verifier struct {
	phase        string
	port         int
	metaClient   *mongo.Client
	srcClient    *mongo.Client
	dstClient    *mongo.Client
	srcBuildInfo *bson.M
	dstBuildInfo *bson.M
	numWorkers   int

	comparisonRetryDelayMillis time.Duration
	workerSleepDelayMillis     time.Duration
	ignoreBSONFieldOrder       bool
	verifyAll                  bool
	startClean                 bool
	partitionSizeInBytes       int64
	readPreference             *readpref.ReadPref

	logger *logger.Logger

	srcNamespaces []string
	dstNamespaces []string
	metaDBName    string
	srcStartAtTs  *primitive.Timestamp

	changeStreamMux       sync.Mutex
	changeStreamRunning   bool
	changeStreamEnderChan chan struct{}
	changeStreamErrChan   chan error
	changeStreamDoneChan  chan struct{}
}

// VerificationStatus holds the Verification Status
type VerificationStatus struct {
	TotalTasks            int `json:"totalTasks"`
	AddedTasks            int `json:"addedTasks"`
	ProcessingTasks       int `json:"processingTasks"`
	FailedTasks           int `json:"failedTasks"`
	CompletedTasks        int `json:"completedTasks"`
	MetadataMismatchTasks int `json:"metadataMismatchTasks"`
	RecheckTasks          int `json:"recheckTasks"`
}

// VerificationResult holds the Verification Results
type VerificationResult struct {
	ID        interface{}
	Field     interface{}
	Type      interface{}
	Details   interface{}
	Cluster   interface{}
	NameSpace interface{}
}

// NewVerifier creates a new Verifier
func NewVerifier() *Verifier {
	return &Verifier{
		phase:                 Idle,
		numWorkers:            NumWorkers,
		readPreference:        readpref.Primary(),
		changeStreamEnderChan: make(chan struct{}),
		changeStreamErrChan:   make(chan error),
		changeStreamDoneChan:  make(chan struct{}),
	}
}

func (verifier *Verifier) getClientOpts(uri string) *options.ClientOptions {
	appName := "Migration Verifier"
	opts := &options.ClientOptions{
		AppName: &appName,
	}
	opts.ApplyURI(uri)
	opts.SetReadConcern(readconcern.Majority())
	opts.SetWriteConcern(writeconcern.New(writeconcern.WMajority()))
	return opts
}

func (verifier *Verifier) GetLogger() *logger.Logger {
	return verifier.logger
}

func (verifier *Verifier) SetMetaURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.metaClient, err = mongo.Connect(ctx, opts)
	return err
}

func (verifier *Verifier) SetSrcURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.srcClient, err = mongo.Connect(ctx, opts)
	if err != nil {
		return err
	}
	verifier.srcBuildInfo, err = getBuildInfo(ctx, verifier.srcClient)
	return err
}

func (verifier *Verifier) SetDstURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.dstClient, err = mongo.Connect(ctx, opts)
	if err != nil {
		return err
	}
	verifier.dstBuildInfo, err = getBuildInfo(ctx, verifier.dstClient)
	return err
}

func (verifier *Verifier) SetServerPort(port int) {
	verifier.port = port
}

func (verifier *Verifier) SetNumWorkers(arg int) {
	verifier.numWorkers = arg
}

func (verifier *Verifier) SetComparisonRetryDelayMillis(arg time.Duration) {
	verifier.comparisonRetryDelayMillis = arg
}

func (verifier *Verifier) SetWorkerSleepDelayMillis(arg time.Duration) {
	verifier.workerSleepDelayMillis = arg
}

func (verifier *Verifier) SetPartitionSizeMB(partitionSizeMB int64) {
	verifier.partitionSizeInBytes = partitionSizeMB * 1024 * 1024
}

func (verifier *Verifier) SetLogger(logPath string) (*os.File, *bufio.Writer, error) {
	var file *os.File
	var writer *bufio.Writer = nil
	if logPath == "stderr" {
		l := zerolog.New(os.Stderr).With().Timestamp().Logger()
		verifier.logger = logger.NewLogger(&l, logger.DefaultLogWriter)
		return file, writer, nil
	}
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		mkdirErr := os.MkdirAll(path.Dir(logPath), 0770)
		if mkdirErr != nil {
			return nil, nil, mkdirErr
		}
	}
	file, err := os.Create(logPath)
	if err != nil {
		return nil, nil, err
	}
	writer = bufio.NewWriter(file)
	l := zerolog.New(writer).With().Timestamp().Logger()
	verifier.logger = logger.NewLogger(&l, logger.DefaultLogWriter)
	return file, writer, nil
}

func (verifier *Verifier) SetSrcNamespaces(arg []string) {
	verifier.srcNamespaces = arg
}

func (verifier *Verifier) SetDstNamespaces(arg []string) {
	verifier.dstNamespaces = arg
}

func (verifier *Verifier) SetMetaDBName(arg string) {
	verifier.metaDBName = arg
}

func (verifier *Verifier) SetIgnoreBSONFieldOrder(arg bool) {
	verifier.ignoreBSONFieldOrder = arg
}

func (verifier *Verifier) SetVerifyAll(arg bool) {
	verifier.verifyAll = arg
}

func (verifier *Verifier) SetStartClean(arg bool) {
	verifier.startClean = arg
}

func (verifier *Verifier) SetReadPreference(arg string) error {
	mode, err := readpref.ModeFromString(arg)
	if err != nil {
		return err
	}
	verifier.readPreference, err = readpref.New(mode)
	return err
}

// DocumentStats gets various status (TODO clarify)
func DocumentStats(ctx context.Context, client *mongo.Client, namespaces []string) {

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Doc Count", "Database", "Collection"})

	for _, n := range namespaces {
		db, coll := SplitNamespace(n)
		if db != "" {
			s, _ := client.Database(db).Collection(coll).EstimatedDocumentCount(ctx)
			table.Append([]string{strconv.FormatInt(s, 10), db, coll})
		}
	}
	table.Render()
	fmt.Println()
}

func (verifier *Verifier) getDocuments(ctx context.Context, collection *mongo.Collection, buildInfo *bson.M,
	startAtTs *primitive.Timestamp, task *VerificationTask) (map[interface{}]bson.Raw, error) {
	var findOptions bson.D
	runCommandOptions := options.RunCmd()
	if len(task.Ids) > 0 {
		filter := bson.D{
			bson.E{
				Key:   "_id",
				Value: bson.M{"$in": task.Ids},
			},
		}
		findOptions = bson.D{
			bson.E{"filter", filter},
		}
	} else {
		findOptions = task.QueryFilter.Partition.GetFindOptions(buildInfo)
	}
	// We only support secondary reads during the check phase, because we don't have the correct startAtTs for the
	// recheck phase.  In the future we could get it, it would be the cluster time on each cluster after writes were quiesced.
	if verifier.readPreference.Mode() != readpref.PrimaryMode && verifier.phase == Check {
		runCommandOptions = runCommandOptions.SetReadPreference(verifier.readPreference)
		if startAtTs != nil {
			// In check mode on the source cluster, we never want to read before the change stream start time.
			findOptions = append(findOptions, bson.E{"readConcern", bson.D{
				{"level", "majority"},
				{"afterClusterTime", *startAtTs},
			}})
		}
	}
	findCmd := append(bson.D{{"find", collection.Name()}}, findOptions...)
	verifier.logger.Debug().Msgf("getDocuments findCmd: %s opts: %v", findCmd, *runCommandOptions)

	cursor, err := collection.Database().RunCommandCursor(ctx, findCmd, runCommandOptions)
	if err != nil {
		return nil, err
	}
	var bytesReturned int64
	bytesReturned, nDocumentsReturned := 0, 0
	documentMap := make(map[interface{}]bson.Raw)
	for cursor.Next(ctx) {
		nDocumentsReturned++
		err := cursor.Err()
		if err != nil {
			return nil, err
		}
		var rawDoc bson.Raw
		err = cursor.Decode(&rawDoc)
		if err != nil {
			return nil, err
		}
		bytesReturned += (int64)(len(rawDoc))
		data := make(bson.Raw, len(rawDoc))
		copy(data, rawDoc)
		idRaw := data.Lookup("_id")
		idString := RawToString(idRaw)

		documentMap[idString] = data
	}
	verifier.logger.Debug().Msgf("Find returned %d documents containing %d bytes", nDocumentsReturned, bytesReturned)

	return documentMap, nil
}

func (verifier *Verifier) FetchAndCompareDocuments(task *VerificationTask) ([]VerificationResult, error) {
	srcClientMap, dstClientMap, err := verifier.fetchDocuments(task)
	if err != nil {
		return nil, err
	}
	return verifier.compareDocuments(srcClientMap, dstClientMap, task.QueryFilter.Namespace)
}

// This is split out to allow unit testing of fetching separate from comparison.
func (verifier *Verifier) fetchDocuments(task *VerificationTask) (map[interface{}]bson.Raw, map[interface{}]bson.Raw, error) {

	var srcClientMap, dstClientMap map[interface{}]bson.Raw
	var srcErr, dstErr error
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	wg.Add(2)
	go func() {
		defer wg.Done()
		srcClientMap, srcErr = verifier.getDocuments(ctx, verifier.srcClientCollection(task), verifier.srcBuildInfo,
			verifier.srcStartAtTs, task)
		if srcErr != nil {
			cancel()
		}
	}()
	go func() {
		defer wg.Done()
		dstClientMap, dstErr = verifier.getDocuments(ctx, verifier.dstClientCollection(task), verifier.dstBuildInfo,
			nil /*startAtTs*/, task)
		if dstErr != nil {
			cancel()
		}
	}()
	if srcErr != nil {
		return nil, nil, srcErr
	}
	if dstErr != nil {
		return nil, nil, srcErr
	}
	return srcClientMap, dstClientMap, nil
}

func (verifier *Verifier) compareDocuments(srcClientMap, dstClientMap map[interface{}]bson.Raw, namespace string) ([]VerificationResult, error) {
	var mismatchedIds []VerificationResult
	for id, srcClientDoc := range srcClientMap {
		dstClientDoc, ok := dstClientMap[id]
		if !ok {
			//verifier.logger.Info().Msgf("Document %+v missing on dstClient!", id)
			mismatchedIds = append(mismatchedIds, VerificationResult{
				ID:        srcClientDoc.Lookup("_id"),
				Details:   Missing,
				Cluster:   ClusterTarget,
				NameSpace: namespace,
			})
			continue
		}

		misMatch, err := verifier.compareOneDocument(srcClientDoc, dstClientDoc, namespace)
		if len(misMatch) > 0 || err != nil {
			mismatchedIds = append(mismatchedIds, misMatch...)
		}
	}

	if len(srcClientMap) != len(dstClientMap) {
		for id, dstClientDoc := range dstClientMap {
			_, ok := srcClientMap[id]
			if !ok {
				//verifier.logger.Info().Msgf("Document %+v missing on srcClient!", id)
				mismatchedIds = append(mismatchedIds, VerificationResult{
					ID:        dstClientDoc.Lookup("_id"),
					Details:   Missing,
					Cluster:   ClusterSource,
					NameSpace: namespace,
				})
			}
		}
	}

	return mismatchedIds, nil
}

func mismatchResultsToVerificationResults(mismatch *MismatchDetails, srcClientDoc, dstClientDoc bson.Raw, namespace string, id interface{}, fieldPrefix string) (results []VerificationResult) {
	for _, field := range mismatch.missingFieldOnSrc {
		result := VerificationResult{
			Field:     fieldPrefix + field,
			Details:   Missing,
			Cluster:   ClusterSource,
			NameSpace: namespace}
		if id != nil {
			result.ID = id
		}
		results = append(results, result)
	}

	for _, field := range mismatch.missingFieldOnDst {
		result := VerificationResult{
			Field:     fieldPrefix + field,
			Details:   Missing,
			Cluster:   ClusterTarget,
			NameSpace: namespace}
		if id != nil {
			result.ID = id
		}
		results = append(results, result)
	}

	for _, field := range mismatch.fieldContentsDiffer {
		srcClientValue := srcClientDoc.Lookup(field)
		dstClientValue := dstClientDoc.Lookup(field)
		details := Mismatch + fmt.Sprintf(" : Document %s failed comparison on field %s between srcClient (Type: %s) and dstClient (Type: %s)", id, fieldPrefix+field, srcClientValue.Type, dstClientValue.Type)
		result := VerificationResult{
			Field:     fieldPrefix + field,
			Details:   details,
			Cluster:   ClusterTarget,
			NameSpace: namespace}
		if id != nil {
			result.ID = id
		}
		results = append(results, result)
	}
	return
}

func (verifier *Verifier) compareOneDocument(srcClientDoc, dstClientDoc bson.Raw, namespace string) ([]VerificationResult, error) {
	match := bytes.Equal(srcClientDoc, dstClientDoc)
	if match {
		return nil, nil
	}
	//verifier.logger.Info().Msg("Byte comparison failed for id %s, falling back to field comparison", id)

	id := srcClientDoc.Lookup("_id")
	if verifier.ignoreBSONFieldOrder {
		mismatch, err := BsonUnorderedCompareRawDocumentWithDetails(srcClientDoc, dstClientDoc)
		if err != nil {
			return nil, err
		}
		if mismatch == nil {
			return nil, nil
		}
		results := mismatchResultsToVerificationResults(mismatch, srcClientDoc, dstClientDoc, namespace, id, "" /* fieldPrefix */)
		return results, nil
	}
	// If we're respecting field order we have just done a binary compare so don't know the mismatching fields.
	return []VerificationResult{{
		ID:        dstClientDoc.Lookup("_id"),
		Details:   Mismatch,
		Cluster:   ClusterTarget,
		NameSpace: namespace,
	}}, nil
}

func (verifier *Verifier) ProcessVerifyTask(workerNum int, task *VerificationTask) {
	verifier.logger.Info().Msgf("[Worker %d] Processing verify task", workerNum)

	mismatches, err := verifier.FetchAndCompareDocuments(task)

	if err != nil {
		task.Status = verificationTaskFailed
		verifier.logger.Error().Msgf("[Worker %d] Error comparing docs: %+v", workerNum, err)
	} else if len(mismatches) == 0 {
		task.Status = verificationTaskCompleted
	} else {
		task.Status = verificationTaskFailed
		verifier.logger.Error().Msgf("[Worker %d] Verification Task %+v failed during Check, may pass Recheck", workerNum, task.PrimaryKey)
		dbName, collName := SplitNamespace(task.QueryFilter.Namespace)
		for _, v := range mismatches {
			err := verifier.InsertFailedCompareRecheckDoc(dbName, collName, v.ID)
			if err != nil {
				verifier.logger.Error().Msgf("[Worker %d] Error inserting docmument mismatch into Recheck queue: %+v", workerNum, err)
			}
		}
	}

	err = verifier.UpdateVerificationTask(task)
	if err != nil {
		verifier.logger.Error().Msgf("Failed updating verification status: %v", err)
	}
}

func (verifier *Verifier) logChunkInfo(ctx context.Context, namespaceAndUUID *uuidutil.NamespaceAndUUID) {
	// Only log full chunk info in debug mode
	debugMsg := verifier.logger.Debug()
	if !debugMsg.Enabled() {
		return
	}
	debugMsg.Discard()

	uuid := namespaceAndUUID.UUID
	namespace := namespaceAndUUID.DBName + "." + namespaceAndUUID.CollName
	configChunkColl := verifier.srcClientDatabase("config").Collection("chunks")
	cursor, err := configChunkColl.Find(ctx, bson.D{{"uuid", uuid}})
	if err != nil {
		verifier.logger.Error().Msgf("Unable to read chunk info for %s: %v", namespace, err)
		return
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var result bson.D
		if err = cursor.Decode(&result); err != nil {
			verifier.logger.Error().Msgf("Error decoding chunk info for %s: %v", namespace, err)
			return
		}
		resultMap := result.Map()
		verifier.logger.Debug().Msgf(" Chunk of %s on %v, range %v to %v", namespace, resultMap["shard"],
			resultMap["min"], resultMap["max"])
	}
	if err = cursor.Err(); err != nil {
		verifier.logger.Error().Msgf("Error reading chunk info for %s: %v", namespace, err)
	}
}

func (verifier *Verifier) logShardingInfo(ctx context.Context, namespaceAndUUID *uuidutil.NamespaceAndUUID) {
	uuid := namespaceAndUUID.UUID
	namespace := namespaceAndUUID.DBName + "." + namespaceAndUUID.CollName
	configCollectionsColl := verifier.srcClientDatabase("config").Collection("collections")
	cursor, err := configCollectionsColl.Find(ctx, bson.D{{"uuid", uuid}})
	if err != nil {
		verifier.logger.Error().Msgf("Unable to read sharding info for %s: %v", namespace, err)
		return
	}
	defer cursor.Close(ctx)
	collectionSharded := false
	for cursor.Next(ctx) {
		collectionSharded = true
		var result bson.D
		if err = cursor.Decode(&result); err != nil {
			verifier.logger.Error().Msgf("Error decoding sharding info for %s: %v", namespace, err)
			return
		}
		resultMap := result.Map()
		verifier.logger.Info().Msgf("Collection %s is sharded with shard key %v", namespace, resultMap["key"])
	}
	if err = cursor.Err(); err != nil {
		verifier.logger.Error().Msgf("Error reading sharding info for %s: %v", namespace, err)
	}
	if collectionSharded {
		verifier.logChunkInfo(ctx, namespaceAndUUID)
	}
}

func (verifier *Verifier) getCollectionPartitions(ctx context.Context, namespace string) ([]*partitions.Partition, error) {
	retryer := retry.New(retry.DefaultDurationLimit).SetRetryOnUUIDNotSupported()
	dbName, collName := SplitNamespace(namespace)
	namespaceAndUUID, err := uuidutil.GetCollectionNamespaceAndUUID(ctx, verifier.logger, retryer,
		verifier.srcClientDatabase(dbName), collName)
	if err != nil {
		return nil, err
	}
	verifier.logShardingInfo(ctx, namespaceAndUUID)

	// The partitioner doles out ranges to replicators; we don't use that functionality so we just pass
	// one "replicator".
	replicator1 := partitions.Replicator{ID: "verifier"}
	replicators := []partitions.Replicator{replicator1}
	partitionList, err := partitions.PartitionCollectionWithSize(
		ctx, namespaceAndUUID, retryer, verifier.srcClient, replicators, verifier.logger, verifier.partitionSizeInBytes)
	if err != nil {
		return nil, err
	}
	// TODO: Test the empty collection (which returns no partitions)
	if len(partitionList) == 0 {
		partitionList = []*partitions.Partition{{
			Key: partitions.PartitionKey{
				SourceUUID:  namespaceAndUUID.UUID,
				MongosyncID: "verifier"},
			Ns: &partitions.Namespace{
				DB:   namespaceAndUUID.DBName,
				Coll: namespaceAndUUID.CollName}}}
	}
	// Use "open" partitions, otherwise out-of-range keys on the destination might be missed
	partitionList[0].Key.Lower = primitive.MinKey{}
	partitionList[len(partitionList)-1].Upper = primitive.MaxKey{}
	debugLog := verifier.logger.Debug()
	if debugLog.Enabled() {
		debugLog.Msgf("Partitions (%d):", len(partitionList))
		for i, partition := range partitionList {
			verifier.logger.Debug().Msgf("Partition %d: %+v -> %+v", i, partition.Key.Lower, partition.Upper)
		}
	}
	rand.Shuffle(len(partitionList), func(i, j int) {
		tmp := partitionList[i]
		partitionList[i] = partitionList[j]
		partitionList[j] = tmp
	})
	debugLog = verifier.logger.Debug()
	if debugLog.Enabled() {
		debugLog.Msgf("Shuffled Partitions (%d):", len(partitionList))
		for i, partition := range partitionList {
			verifier.logger.Debug().Msgf("Shuffled Partition %d: %+v -> %+v", i, partition.Key.Lower, partition.Upper)
		}
	}

	return partitionList, nil
}

func (verifier *Verifier) getCollectionSpecification(ctx context.Context, collection *mongo.Collection) (*mongo.CollectionSpecification, error) {
	filter := bson.D{{"name", collection.Name()}}
	specifications, err := collection.Database().ListCollectionSpecifications(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(specifications) > 1 {
		return nil, errors.Errorf("Too many collections named %s %+v", FullName(collection), specifications)
	}
	if len(specifications) == 1 {
		verifier.logger.Debug().Msgf("Collection specification: %+v", specifications[0])
		return specifications[0], nil
	}

	return nil, nil
}

// Returns a slice of VerificationResults with the differences, and a boolean indicating whether or
// not the collection data can be safely verified.
func (verifier *Verifier) compareCollectionSpecifications(srcNs string, dstNs string, srcSpec *mongo.CollectionSpecification, dstSpec *mongo.CollectionSpecification) ([]VerificationResult, bool) {
	if srcSpec == nil {
		return []VerificationResult{{
			NameSpace: srcNs,
			Cluster:   ClusterSource,
			Details:   Missing}}, false
	}
	if dstSpec == nil {
		return []VerificationResult{{
			NameSpace: dstNs,
			Cluster:   ClusterTarget,
			Details:   Missing}}, false
	}
	if srcSpec.Type != dstSpec.Type {
		return []VerificationResult{{
			NameSpace: srcNs,
			Cluster:   ClusterTarget,
			Field:     "Type",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Type, dstSpec.Type)}}, false
		// If the types differ, the rest is not important.
	}
	var results []VerificationResult
	if srcSpec.ReadOnly != dstSpec.ReadOnly {
		results = append(results, VerificationResult{
			NameSpace: dstNs,
			Cluster:   ClusterTarget,
			Field:     "ReadOnly",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.ReadOnly, dstSpec.ReadOnly)})
	}
	if !bytes.Equal(srcSpec.Options, dstSpec.Options) {
		mismatchDetails, err := BsonUnorderedCompareRawDocumentWithDetails(srcSpec.Options, dstSpec.Options)
		if err != nil {
			verifier.logger.Error().Msgf("Unable to parse collection options for %s: %+v", srcNs, err)
			results = append(results, VerificationResult{
				NameSpace: dstNs,
				Cluster:   ClusterTarget,
				Field:     "Options",
				Details:   "ParseError " + fmt.Sprintf("%v", err)})
			return results, false
		}
		if mismatchDetails == nil {
			results = append(results, VerificationResult{
				NameSpace: dstNs,
				Cluster:   ClusterTarget,
				Field:     "Options (Field Order Only)",
				Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Options, dstSpec.Options)})
		} else {
			results = append(results, mismatchResultsToVerificationResults(mismatchDetails, srcSpec.Options, dstSpec.Options, srcNs, nil /* id */, "Options.")...)
		}
	}
	// Do not compare data between capped and uncapped collections because the partitioning is different.
	canCompareData := srcSpec.Options.Lookup("capped").Equal(dstSpec.Options.Lookup("capped"))

	return results, canCompareData
}

func compareIndexSpecifications(srcSpec *mongo.IndexSpecification, dstSpec *mongo.IndexSpecification) []VerificationResult {
	var results []VerificationResult
	// Order is always significant in the keys document.
	if !bytes.Equal(srcSpec.KeysDocument, dstSpec.KeysDocument) {
		results = append(results, VerificationResult{
			NameSpace: dstSpec.Namespace,
			Cluster:   ClusterTarget,
			ID:        dstSpec.Name,
			Field:     "KeysDocument",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.KeysDocument, dstSpec.KeysDocument)})
	}

	// We don't check version because it may change when migrating between server versions.

	if !reflect.DeepEqual(srcSpec.ExpireAfterSeconds, dstSpec.ExpireAfterSeconds) {
		results = append(results, VerificationResult{
			NameSpace: dstSpec.Namespace,
			Cluster:   ClusterTarget,
			ID:        dstSpec.Name,
			Field:     "ExpireAfterSeconds",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.ExpireAfterSeconds, dstSpec.ExpireAfterSeconds)})
	}

	if !reflect.DeepEqual(srcSpec.Sparse, dstSpec.Sparse) {
		results = append(results, VerificationResult{
			NameSpace: dstSpec.Namespace,
			Cluster:   ClusterTarget,
			ID:        dstSpec.Name,
			Field:     "Sparse",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Sparse, dstSpec.Sparse)})
	}

	if !reflect.DeepEqual(srcSpec.Unique, dstSpec.Unique) {
		results = append(results, VerificationResult{
			NameSpace: dstSpec.Namespace,
			Cluster:   ClusterTarget,
			ID:        dstSpec.Name,
			Field:     "Unique",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Unique, dstSpec.Unique)})
	}

	if !reflect.DeepEqual(srcSpec.Clustered, dstSpec.Clustered) {
		results = append(results, VerificationResult{
			NameSpace: dstSpec.Namespace,
			Cluster:   ClusterTarget,
			ID:        dstSpec.Name,
			Field:     "Clustered",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Clustered, dstSpec.Clustered)})
	}
	return results
}

func (verifier *Verifier) ProcessCollectionVerificationTask(ctx context.Context, workerNum int, task *VerificationTask) {
	verifier.logger.Info().Msgf("[Worker %d] Processing collection", workerNum)
	verifier.verifyMetadataAndPartitionCollection(ctx, workerNum, task)
	err := verifier.UpdateVerificationTask(task)
	if err != nil {
		verifier.logger.Error().Msgf("Failed updating verification status: %v", err)
	}
}

func (verifier *Verifier) markCollectionFailed(workerNum int, task *VerificationTask, cluster string, namespace string, err error) {
	task.Status = verificationTaskFailed
	verifier.logger.Error().Msgf("[Worker %d] Unable to read metadata for collection %s from cluster %s: %+v", workerNum, namespace, cluster, err)
	task.FailedDocs = append(task.FailedDocs, VerificationResult{
		NameSpace: namespace,
		Cluster:   cluster,
		Details:   Failed + fmt.Sprintf(" %v", err)})
}

func verifyIndexes(ctx context.Context, workerNum int, task *VerificationTask, srcColl, dstColl *mongo.Collection,
	srcIdIndexSpec, dstIdIndexSpec *mongo.IndexSpecification) ([]VerificationResult, error) {
	srcSpecs, err := srcColl.Indexes().ListSpecifications(ctx)
	if err != nil {
		return nil, err
	}
	if srcIdIndexSpec != nil {
		srcSpecs = append(srcSpecs, srcIdIndexSpec)
	}
	dstSpecs, err := dstColl.Indexes().ListSpecifications(ctx)
	if err != nil {
		return nil, err
	}
	if dstIdIndexSpec != nil {
		dstSpecs = append(dstSpecs, dstIdIndexSpec)
	}
	var results []VerificationResult
	srcMap := map[string](*mongo.IndexSpecification){}
	srcMapUsed := map[string]bool{}
	for _, srcSpec := range srcSpecs {
		srcMap[srcSpec.Name] = srcSpec
	}
	for _, dstSpec := range dstSpecs {
		srcSpec := srcMap[dstSpec.Name]
		if srcSpec == nil {
			results = append(results, VerificationResult{
				ID:        dstSpec.Name,
				Details:   Missing,
				Cluster:   ClusterSource,
				NameSpace: FullName(srcColl)})
		} else {
			srcMapUsed[srcSpec.Name] = true
			compareSpecResults := compareIndexSpecifications(srcSpec, dstSpec)
			if compareSpecResults != nil {
				results = append(results, compareSpecResults...)
			}
		}
	}

	// Find any index specs which existed in the source cluster but not the target cluster.
	for _, srcSpec := range srcSpecs {
		if !srcMapUsed[srcSpec.Name] {
			results = append(results, VerificationResult{
				ID:        srcSpec.Name,
				Details:   Missing,
				Cluster:   ClusterTarget,
				NameSpace: FullName(dstColl)})
		}
	}
	return results, nil
}

func (verifier *Verifier) verifyMetadataAndPartitionCollection(ctx context.Context, workerNum int, task *VerificationTask) {
	srcColl := verifier.srcClientCollection(task)
	dstColl := verifier.dstClientCollection(task)
	srcNs := FullName(srcColl)
	dstNs := FullName(dstColl)
	srcSpec, srcErr := verifier.getCollectionSpecification(ctx, srcColl)
	dstSpec, dstErr := verifier.getCollectionSpecification(ctx, dstColl)
	if srcErr != nil {
		verifier.markCollectionFailed(workerNum, task, ClusterSource, srcNs, srcErr)
	}
	if dstErr != nil {
		verifier.markCollectionFailed(workerNum, task, ClusterTarget, dstNs, dstErr)
	}
	if srcErr != nil || dstErr != nil {
		return
	}
	if srcSpec == nil && dstSpec == nil {
		verifier.logger.Info().Msgf("[Worker %d] Collection not present on either cluster: %s -> %s", workerNum, srcNs, dstNs)
		// This counts as success.
		task.Status = verificationTaskCompleted
		return
	}
	specificationProblems, verifyData := verifier.compareCollectionSpecifications(srcNs, dstNs, srcSpec, dstSpec)
	if specificationProblems != nil {
		task.FailedDocs = specificationProblems
		if !verifyData {
			task.Status = verificationTaskFailed
			return
		}
		task.Status = verificationTaskMetadataMismatch
	}

	indexProblems, err := verifyIndexes(ctx, workerNum, task, srcColl, dstColl, srcSpec.IDIndex, dstSpec.IDIndex)
	if err != nil {
		task.Status = verificationTaskFailed
		verifier.logger.Error().Msgf("[Worker %d] Error getting indexes for collection: %+v", workerNum, err)
		return
	}
	if indexProblems != nil {
		task.FailedDocs = append(task.FailedDocs, indexProblems...)
		task.Status = verificationTaskMetadataMismatch
	}

	partitions, err := verifier.getCollectionPartitions(ctx, srcNs)
	if err != nil {
		task.Status = verificationTaskFailed
		verifier.logger.Error().Msgf("[Worker %d] Error partitioning collection: %+v", workerNum, err)
		return
	}
	verifier.logger.Info().Msgf("[Worker %d] split collection info %d partitions", workerNum, len(partitions))
	for _, partition := range partitions {
		_, err := InsertPartitionVerificationTask(partition, dstNs, verifier.verificationTaskCollection())
		if err != nil {
			task.Status = verificationTaskFailed
			verifier.logger.Error().Msgf("[Worker %d] Error inserting verifier tasks: %+v", workerNum, err)
		}
	}
	if task.Status == verificationTaskProcessing {
		task.Status = verificationTaskCompleted
	}
}

func (verifier *Verifier) Work(workerNum int, ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	verifier.logger.Info().Msgf("[Worker %d] Started", workerNum)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			task, err := verifier.FindNextVerifyTaskAndUpdate()
			if errors.Is(err, mongo.ErrNoDocuments) {
				verifier.logger.Info().Msgf("[Worker %d] No tasks found, sleeping...", workerNum)
				time.Sleep(verifier.workerSleepDelayMillis * time.Millisecond)
				continue
			} else if err != nil {
				panic(err)
			}
			if task.Type == verificationTaskVerifyCollection {
				verifier.ProcessCollectionVerificationTask(ctx, workerNum, task)
			} else {
				verifier.ProcessVerifyTask(workerNum, task)
			}
		}
	}
}

func (verifier *Verifier) GetVerificationStatus() (*VerificationStatus, error) {
	ctx := context.Background()
	verificationStatus := VerificationStatus{}
	taskCollection := verifier.verificationTaskCollection()

	aggregation := []bson.M{
		{
			"$match": bson.M{
				"type": bson.M{"$ne": "primary"},
			},
		},
		{
			"$group": bson.M{
				"_id":   "$status",
				"count": bson.M{"$sum": 1},
			},
		},
	}
	cursor, err := taskCollection.Aggregate(ctx, aggregation)
	if err != nil {
		return nil, err
	}
	var results []bson.Raw
	err = cursor.All(ctx, &results)
	if err != nil {
		return nil, err
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Status", "Count"})

	for _, result := range results {
		status := result.Lookup("_id").String()
		// Status is returned with quotes around it so remove those
		status = status[1 : len(status)-1]
		count := int(result.Lookup("count").Int32())
		verificationStatus.TotalTasks += int(count)
		switch status {
		case verificationTaskAdded:
			verificationStatus.AddedTasks = count
		case verificationTaskProcessing:
			verificationStatus.ProcessingTasks = count
		case verificationTaskFailed:
			verificationStatus.FailedTasks = count
		case verificationTaskMetadataMismatch:
			verificationStatus.MetadataMismatchTasks = count
		case verificationTaskCompleted:
			verificationStatus.CompletedTasks = count
		case verificationTasksRetry:
			verificationStatus.RecheckTasks = count
		default:
			verifier.logger.Info().Msgf("Unknown task status %s", status)
		}

		table.Append([]string{status, strconv.Itoa(count)})
	}
	fmt.Println("\nVerify Tasks:")
	table.Render()
	fmt.Println()

	return &verificationStatus, nil
}

func (verifier *Verifier) verificationDatabase() *mongo.Database {
	db := verifier.metaClient.Database(verifier.metaDBName)
	if db.WriteConcern().GetW() != "majority" {
		verifier.logger.Fatal().Msgf("Verification metadata is not using write concern majority: %+v", db.WriteConcern())
	}
	if db.ReadConcern().GetLevel() != "majority" {
		verifier.logger.Fatal().Msgf("Verification metadata is not using read concern majority: %+v", db.ReadConcern())
	}
	return db
}

func (verifier *Verifier) verificationTaskCollection() *mongo.Collection {
	return verifier.verificationDatabase().Collection(verificationTasksCollection)
}

func (verifier *Verifier) verificationRangeCollection() *mongo.Collection {
	return verifier.verificationDatabase().Collection(verificationRangeCollection)
}

func (verifier *Verifier) refetchCollection() *mongo.Collection {
	return verifier.verificationDatabase().Collection(refetch)
}

func (verifier *Verifier) srcClientDatabase(dbName string) *mongo.Database {
	db := verifier.srcClient.Database(dbName)
	// No need to check the write concern because we do not write to the source database.
	if db.ReadConcern().GetLevel() != "majority" {
		verifier.logger.Fatal().Msgf("Source client is not using read concern majority: %+v", db.ReadConcern())
	}
	return db
}

func (verifier *Verifier) dstClientDatabase(dbName string) *mongo.Database {
	db := verifier.dstClient.Database(dbName)
	// No need to check the write concern because we do not write to the target database.
	if db.ReadConcern().GetLevel() != "majority" {
		verifier.logger.Fatal().Msgf("Source client is not using read concern majority: %+v", db.ReadConcern())
	}
	return db
}

func (verifier *Verifier) metaClientCollection(name string) *mongo.Collection {
	return verifier.metaClient.Database(verifier.metaDBName).Collection(name)
}

func (verifier *Verifier) srcClientCollection(task *VerificationTask) *mongo.Collection {
	if task != nil {
		dbName, collName := SplitNamespace(task.QueryFilter.Namespace)
		return verifier.srcClientDatabase(dbName).Collection(collName)
	}
	return nil
}

func (verifier *Verifier) dstClientCollection(task *VerificationTask) *mongo.Collection {
	if task != nil {
		if task.QueryFilter.To != "" {
			return verifier.dstClientCollectionByNameSpace(task.QueryFilter.To)
		} else {
			return verifier.dstClientCollectionByNameSpace(task.QueryFilter.Namespace)
		}
	}
	return nil
}

func (verifier *Verifier) dstClientCollectionByNameSpace(namespace string) *mongo.Collection {
	dbName, collName := SplitNamespace(namespace)
	return verifier.dstClientDatabase(dbName).Collection(collName)
}

func (verifier *Verifier) StartServer() error {
	server := NewWebServer(verifier.port, verifier, verifier.logger)
	return server.Run(context.Background())
}

func (verifier *Verifier) GetProgress(ctx context.Context) (Progress, error) {
	status, err := verifier.GetVerificationStatus()
	if err != nil {
		return Progress{Error: err}, err
	}
	return Progress{
		Phase:  verifier.phase,
		Status: status,
	}, nil
}

func (verifier *Verifier) Check(ctx context.Context) error {
	var err error
	if verifier.startClean {
		verifier.logger.Info().Msg("Dropping old verifier metadata")
		err = verifier.verificationDatabase().Drop(ctx)
		if err != nil {
			return err
		}
	}
	verifier.logger.Info().Msg("Starting Check")

	verifier.phase = Check
	defer func() {
		verifier.phase = Idle
	}()

	verifier.changeStreamMux.Lock()
	csRunning := verifier.changeStreamRunning
	verifier.changeStreamMux.Unlock()
	if !csRunning {
		verifier.logger.Info().Msg("Change stream not running, starting change stream")
		retryer := retry.New(retry.DefaultDurationLimit).SetRetryOnUUIDNotSupported()
		startAtTs, err := GetLastOpTimeAndSyncShardClusterTime(ctx,
			verifier.logger,
			retryer,
			verifier.srcClient,
			true)
		if err != nil {
			return err
		}
		err = verifier.StartChangeStream(ctx, startAtTs)
		if err != nil {
			return err
		}
		verifier.srcStartAtTs = startAtTs
	}

	// Log out the verification status when initially booting up so it's easy to see the current state
	verificationStatus, err := verifier.GetVerificationStatus()
	if err != nil {
		verifier.logger.Error().Msgf("Failed getting verification status: %v", err)
	} else {
		verifier.logger.Info().Msgf("Initial verification status: %+v", verificationStatus)
	}

	err = verifier.CreateInitialTasks()
	if err != nil {
		return err
	}

	verifier.logger.Info().Msgf("Starting %d verification workers", verifier.numWorkers)
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	for i := 0; i < verifier.numWorkers; i++ {
		wg.Add(1)
		go verifier.Work(i, ctx, &wg)
		time.Sleep(10 * time.Millisecond)
	}

	waitForTaskCreation := 0
	for {
		select {
		case err := <-verifier.changeStreamErrChan:
			cancel()
			return err
		case <-ctx.Done():
			cancel()
			return nil
		default:
		}

		verificationStatus, err := verifier.GetVerificationStatus()
		if err != nil {
			verifier.logger.Error().Msgf("Failed getting verification status: %v", err)
		}

		if waitForTaskCreation%2 == 0 {
			verifier.PrintVerificationSummary(ctx)
		}

		//wait for task to be created, if none of the tasks found.
		if verificationStatus.AddedTasks > 0 || verificationStatus.ProcessingTasks > 0 || verificationStatus.RecheckTasks > 0 {
			waitForTaskCreation++
			time.Sleep(15 * time.Second)
		} else {
			verifier.PrintVerificationSummary(ctx)
			verifier.logger.Info().Msg("Verification tasks complete")
			cancel()
			wg.Wait()
			break
		}
	}
	verifier.logger.Info().Msg("Check finished")
	return nil
}

func (verifier *Verifier) setupAllNamespaceList(ctx context.Context) error {
	// We want to check all user collections on both source and dest.
	srcNamespaces, err := ListAllUserCollections(ctx, verifier.logger, verifier.srcClient, verifier.metaDBName)
	if err != nil {
		return err
	}

	dstNamespaces, err := ListAllUserCollections(ctx, verifier.logger, verifier.dstClient, verifier.metaDBName)
	if err != nil {
		return err
	}

	srcMap := map[string]bool{}
	for _, ns := range srcNamespaces {
		srcMap[ns] = true
	}
	for _, ns := range dstNamespaces {
		if !srcMap[ns] {
			srcNamespaces = append(srcNamespaces, ns)
		}
	}
	verifier.logger.Info().Msgf("Namespaces to verify %+v", srcNamespaces)
	// In verifyAll mode, we do not support collection renames, so src and dest lists are the same.
	verifier.srcNamespaces = srcNamespaces
	verifier.dstNamespaces = srcNamespaces
	return nil
}

func (verifier *Verifier) CreateInitialTasks() error {
	// If we don't know the src namespaces, we're definitely not the primary task.
	if !verifier.verifyAll {
		if len(verifier.srcNamespaces) == 0 {
			return nil
		}
		if len(verifier.dstNamespaces) == 0 {
			verifier.dstNamespaces = verifier.srcNamespaces
		}
		if len(verifier.srcNamespaces) != len(verifier.dstNamespaces) {
			err := errors.Errorf("Different number of source and destination namespaces")
			verifier.logger.Error().Msgf("%s", err)
			return err
		}
	}
	isPrimary, err := verifier.CheckIsPrimary()
	if err != nil {
		return err
	}
	if !isPrimary {
		return nil
	}
	if verifier.verifyAll {
		err := verifier.setupAllNamespaceList(context.Background())
		if err != nil {
			return err
		}
	}
	for i, src := range verifier.srcNamespaces {
		dst := verifier.dstNamespaces[i]
		verifier.logger.Info().Msgf("Adding task for %s -> %s", src, dst)
		_, err := InsertCollectionVerificationTask(src, dst, verifier.verificationTaskCollection())
		if err != nil {
			verifier.logger.Error().Msgf("Failed to insert collection verification task: %s", err)
			return err
		}
	}

	err = verifier.UpdatePrimaryTaskComplete()
	if err != nil {
		return err
	}
	return nil
}

func FetchFailedTasks(ctx context.Context, coll *mongo.Collection, taskType string) []VerificationTask {

	var FailedTasks []VerificationTask
	status := []string{verificationTasksRetry, verificationTaskFailed, verificationTaskMetadataMismatch}
	cur, err := coll.Find(ctx, bson.D{bson.E{Key: "type", Value: taskType},
		bson.E{Key: "status", Value: bson.M{"$in": status}}})
	if err != nil {
		return FailedTasks
	}

	err = cur.All(ctx, &FailedTasks)
	if err != nil {
		return FailedTasks
	}

	return FailedTasks
}

func (verifier *Verifier) PrintVerificationSummary(ctx context.Context) {

	// cache namespace
	if len(verifier.srcNamespaces) == 0 {
		verifier.srcNamespaces = verifier.getNamespaces(ctx, SrcNamespaceField)
		// if still no namespace, nothing to print!
		if len(verifier.srcNamespaces) == 0 {
			verifier.logger.Info().Msg("Unable to find the namespace to display DB stats.")
			return
		}
	}

	if len(verifier.dstNamespaces) == 0 {
		verifier.dstNamespaces = verifier.getNamespaces(ctx, DstNamespaceField)
		if len(verifier.dstNamespaces) == 0 {
			verifier.dstNamespaces = verifier.srcNamespaces
		}
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Src Count", "Src DB", "Src Coll", "Dst Count", "Dst DB", "Dst Coll"})

	table2 := tablewriter.NewWriter(os.Stdout)
	table2.SetHeader([]string{"Src Count", "Src DB", "Src Coll", "Dst Count", "Dst DB", "Dst Coll"})

	diffCounts := 0
	matchingCounts := 0

	for i, n := range verifier.srcNamespaces {
		srcDb, srcColl := SplitNamespace(n)
		if srcDb != "" {
			srcEst, _ := verifier.srcClientDatabase(srcDb).Collection(srcColl).EstimatedDocumentCount(ctx)

			n2 := verifier.dstNamespaces[i]
			dstDb, dstColl := SplitNamespace(n2)
			if dstDb != "" {
				dstEst, _ := verifier.dstClientDatabase(dstDb).Collection(dstColl).EstimatedDocumentCount(ctx)

				table.Append([]string{strconv.FormatInt(srcEst, 10), srcDb, srcColl, strconv.FormatInt(dstEst, 10), dstDb, dstColl})
				if srcEst != dstEst {
					table2.Append([]string{strconv.FormatInt(srcEst, 10), srcDb, srcColl, strconv.FormatInt(dstEst, 10), dstDb, dstColl})
					diffCounts++
				} else {
					matchingCounts++
				}
			}
		}
	}

	fmt.Printf("Collections with matching counts: %d\n\n", matchingCounts)

	if matchingCounts <= 25 {
		fmt.Println("\nSource / Target Comparison (All collections):")
		table.Render()
		fmt.Print("\n")
	}

	if diffCounts == 0 {
		fmt.Print("********** ALL COLLECTION COUNTS MATCH ! **********\n\n")
	} else {
		fmt.Println("\nSource / Target Comparison (Differing counts only):")
		table2.Render()
		fmt.Print("Differences in counts may be due to query filters\n\n\n")
	}

	metadataFailedTasks := FetchFailedTasks(ctx, verifier.verificationTaskCollection(), verificationTaskVerifyCollection)
	if len(metadataFailedTasks) != 0 {
		table = tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"Index", "Cluster", "Type", "Field", "Namespace", "Details"})

		for _, v := range metadataFailedTasks {
			for _, f := range v.FailedDocs {
				table.Append([]string{fmt.Sprintf("%v", f.ID), fmt.Sprintf("%v", f.Cluster), fmt.Sprintf("%v", f.Type), fmt.Sprintf("%v", f.Field), fmt.Sprintf("%v", f.NameSpace), fmt.Sprintf("%v", f.Details)})
			}
		}
		fmt.Println("Collections/Indexes in failed or retry status:")
		table.Render()
		fmt.Println()
	} else {
		fmt.Print("********** ALL COLLECTION METADATA MATCHES ! **********\n\n")
	}

	FailedTasks := FetchFailedTasks(ctx, verifier.verificationTaskCollection(), verificationTaskVerify)
	if len(FailedTasks) == 0 {
		// Nothing to print
		return
	}

	table = tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"ID", "Cluster", "Type", "Field", "Namespace", "Details"})

	for _, v := range FailedTasks {
		for _, f := range v.FailedDocs {
			table.Append([]string{fmt.Sprintf("%v", f.ID), fmt.Sprintf("%v", f.Cluster), fmt.Sprintf("%v", f.Type), fmt.Sprintf("%v", f.Field), fmt.Sprintf("%v", f.NameSpace), fmt.Sprintf("%v", f.Details)})
		}
	}
	fmt.Println("Documents in tasks in failed or retry status:")
	table.Render()
	fmt.Println()
}

func (verifier *Verifier) getNamespaces(ctx context.Context, fieldName string) []string {
	var namespaces []string
	ret, err := verifier.verificationTaskCollection().Distinct(ctx, fieldName, bson.D{})
	if err != nil {
		return namespaces
	}
	for _, v := range ret {
		namespaces = append(namespaces, v.(string))
	}
	return namespaces
}

func getBuildInfo(ctx context.Context, client *mongo.Client) (*bson.M, error) {
	commandResult := client.Database("admin").RunCommand(ctx, bson.D{{"buildinfo", 1}})
	if commandResult.Err() != nil {
		return nil, commandResult.Err()
	}
	var buildInfo bson.D
	err := commandResult.Decode(&buildInfo)
	if err != nil {
		return nil, err
	}
	buildInfoMap := buildInfo.Map()
	return &buildInfoMap, nil
}
