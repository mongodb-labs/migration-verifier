package verifier

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	_ "net/http/pprof"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/uuidutil"
	"github.com/10gen/migration-verifier/internal/verifier/localdb"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	"github.com/dustin/go-humanize"
	clone "github.com/huandu/go-clone/generic"
	"github.com/olekukonko/tablewriter"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// ReadConcernSetting describes the verifier’s handling of read
// concern.
type ReadConcernSetting string

const (
	//TODO: add comments for each of these so the warnings will stop :)
	Failed            = "Failed"
	Mismatch          = "Mismatch"
	ClusterTarget     = "dstClient"
	ClusterSource     = "srcClient"
	SrcNamespaceField = "query_filter.namespace"
	DstNamespaceField = "query_filter.to"
	NumWorkers        = 10
	Idle              = "idle"
	Check             = "check"
	Recheck           = "recheck"

	// ReadConcernMajority means to force majority read concern.
	// This is generally desirable to ensure consistency.
	ReadConcernMajority ReadConcernSetting = "forceMajority"

	// ReadConcernIgnore means to use connection-default read concerns.
	// This setting’s main purpose is to allow local read concern,
	// which is important for deployments where majority read concern
	// is unworkable.
	ReadConcernIgnore ReadConcernSetting = "ignore"

	DefaultFailureDisplaySize = 20

	okSymbol    = "\u2705" // white heavy check mark
	infoSymbol  = "\u24d8" // circled Latin small letter I
	notOkSymbol = "\u2757" // heavy exclamation mark symbol

	clientAppName = "Migration Verifier"

	progressReportTimeWarnThreshold = 10 * time.Second

	DefaultRecheckMaxSizeMB = 8
	MaxRecheckMaxSizeMB     = 12

	DefaultLocalDBPath = "verifier.db"
)

type whichCluster string

const (
	src whichCluster = "source"
	dst whichCluster = "destination"
)

var timeFormat = time.RFC3339

// Verifier is the main state for the migration verifier
type Verifier struct {
	writesOff          bool
	lastGeneration     bool
	running            bool
	generation         int
	phase              string
	port               int
	metaURI            string
	metaClient         *mongo.Client
	srcClient          *mongo.Client
	dstClient          *mongo.Client
	srcClusterInfo     *util.ClusterInfo
	dstClusterInfo     *util.ClusterInfo
	numWorkers         int
	failureDisplaySize int64

	srcEventRecorder *EventRecorder
	dstEventRecorder *EventRecorder

	// Used only with generation 0 to defer the first
	// progress report until after we’ve finished partitioning
	// every collection.
	gen0PendingCollectionTasks atomic.Int32

	generationStartTime  time.Time
	generationPauseDelay time.Duration
	workerSleepDelay     time.Duration
	docCompareMethod     DocCompareMethod
	verifyAll            bool
	startClean           bool

	// This would seem more ideal as uint64, but changing it would
	// trigger several other similar type changes, and that’s not really
	// worthwhile for now.
	partitionSizeInBytes int64

	recheckMaxSizeInBytes types.ByteCount

	readPreference *readpref.ReadPref

	logger *logger.Logger
	writer io.Writer

	srcNamespaces []string
	dstNamespaces []string
	nsMap         *NSMap
	metaDBName    string

	mux sync.RWMutex

	srcChangeStreamReader *ChangeStreamReader
	dstChangeStreamReader *ChangeStreamReader

	readConcernSetting ReadConcernSetting

	// A user-defined $match-compatible document-level query filter.
	// The filter is applied to all namespaces in both initial checking and iterative checking.
	// The verifier only checks documents within the filter.
	globalFilter bson.D

	pprofInterval time.Duration

	workerTracker *WorkerTracker

	verificationStatusCheckInterval time.Duration

	localDB *localdb.LocalDB
}

var _ MigrationVerifierAPI = &Verifier{}

// VerificationStatus holds the Verification Status
type VerificationStatus struct {
	TotalTasks            int `json:"totalTasks"`
	AddedTasks            int `json:"addedTasks"`
	ProcessingTasks       int `json:"processingTasks"`
	FailedTasks           int `json:"failedTasks"`
	CompletedTasks        int `json:"completedTasks"`
	MetadataMismatchTasks int `json:"metadataMismatchTasks"`
}

// VerifierSettings is NewVerifier’s argument.
type VerifierSettings struct {
	ReadConcernSetting
}

// NewVerifier creates a new Verifier
func NewVerifier(settings VerifierSettings, logPath, dbPath string) *Verifier {
	readConcern := settings.ReadConcernSetting
	if readConcern == "" {
		readConcern = ReadConcernMajority
	}

	logger, logWriter := getLoggerAndWriter(logPath)

	ldb, err := localdb.New(logger, dbPath)
	if err != nil {
		panic(fmt.Sprintf("failed to open local DB %#q: %v", dbPath, err))
	}

	return &Verifier{
		logger: logger,
		writer: logWriter,

		phase:                 Idle,
		numWorkers:            NumWorkers,
		readPreference:        readpref.Primary(),
		partitionSizeInBytes:  400 * 1024 * 1024,
		recheckMaxSizeInBytes: DefaultRecheckMaxSizeMB * 1024 * 1024,
		failureDisplaySize:    DefaultFailureDisplaySize,

		readConcernSetting: readConcern,

		// This will get recreated once gen0 starts, but we want it
		// here in case the change streams gets an event before then.
		srcEventRecorder: NewEventRecorder(),
		dstEventRecorder: NewEventRecorder(),

		workerTracker: NewWorkerTracker(NumWorkers),

		verificationStatusCheckInterval: 15 * time.Second,
		nsMap:                           NewNSMap(),

		localDB: ldb,
	}
}

// ConfigureReadConcern
func (verifier *Verifier) ConfigureReadConcern(setting ReadConcernSetting) {
	verifier.readConcernSetting = setting
}

func (verifier *Verifier) getClientOpts(uri string) *options.ClientOptions {
	appName := clientAppName
	opts := &options.ClientOptions{
		AppName: &appName,
	}
	opts.ApplyURI(uri)
	opts.SetWriteConcern(writeconcern.Majority())

	verifier.doIfForceReadConcernMajority(func() {
		opts.SetReadConcern(readconcern.Majority())
	})

	return opts
}

func (verifier *Verifier) SetFailureDisplaySize(size int64) {
	verifier.failureDisplaySize = size
}

func (verifier *Verifier) WritesOff(ctx context.Context) error {
	verifier.logger.Debug().
		Msg("WritesOff called.")

	var srcFinalTs, dstFinalTs primitive.Timestamp
	var err error

	// The anonymous function here makes it easier to ensure
	// that we unlock the mutex.
	err = func() error {
		verifier.mux.Lock()
		defer verifier.mux.Unlock()

		if verifier.writesOff {
			return errors.New("writesOff already set")
		}
		verifier.writesOff = true

		verifier.logger.Debug().Msg("Signalling that writes are done.")

		srcFinalTs, err = GetNewClusterTime(
			ctx,
			verifier.logger,
			verifier.srcClient,
		)

		if err != nil {
			return errors.Wrapf(err, "failed to fetch source's cluster time")
		}

		dstFinalTs, err = GetNewClusterTime(
			ctx,
			verifier.logger,
			verifier.dstClient,
		)

		if err != nil {
			return errors.Wrapf(err, "failed to fetch destination's cluster time")
		}

		return nil
	}()
	if err != nil {
		return err
	}

	// This has to happen outside the lock because the change streams
	// might be inserting docs into the recheck queue, which happens
	// under the lock.
	select {
	case <-verifier.srcChangeStreamReader.readerError.Ready():
		err := verifier.srcChangeStreamReader.readerError.Get()
		return errors.Wrapf(err, "tried to send writes-off timestamp to %s, but change stream already failed", verifier.srcChangeStreamReader)
	default:
		verifier.srcChangeStreamReader.writesOffTs.Set(srcFinalTs)
	}

	select {
	case <-verifier.dstChangeStreamReader.readerError.Ready():
		err := verifier.dstChangeStreamReader.readerError.Get()
		return errors.Wrapf(err, "tried to send writes-off timestamp to %s, but change stream already failed", verifier.dstChangeStreamReader)
	default:
		verifier.dstChangeStreamReader.writesOffTs.Set(dstFinalTs)
	}

	return nil
}

func (verifier *Verifier) WritesOn(ctx context.Context) {
	verifier.mux.Lock()
	verifier.writesOff = false
	verifier.mux.Unlock()
}

func (verifier *Verifier) GetLogger() *logger.Logger {
	return verifier.logger
}

func (verifier *Verifier) SetMetaURI(ctx context.Context, uri string) error {
	opts := verifier.getClientOpts(uri)
	var err error
	verifier.metaClient, err = mongo.Connect(ctx, opts)
	if err != nil {
		return err
	}

	verifier.metaURI = uri

	return err
}

func (verifier *Verifier) AddMetaIndexes(ctx context.Context) error {
	model := mongo.IndexModel{Keys: bson.M{"generation": 1}}
	_, err := verifier.verificationTaskCollection().Indexes().CreateOne(ctx, model)

	if err != nil {
		return errors.Wrapf(err, "creating generation index")
	}

	err = createMismatchesCollection(
		ctx,
		verifier.verificationDatabase(),
	)

	return errors.Wrapf(err, "creating discrepancies collection")
}

func (verifier *Verifier) SetServerPort(port int) {
	verifier.port = port
}

func (verifier *Verifier) SetNumWorkers(arg int) {
	verifier.numWorkers = arg
}

func (verifier *Verifier) SetGenerationPauseDelay(arg time.Duration) {
	verifier.generationPauseDelay = arg
}

func (verifier *Verifier) SetWorkerSleepDelay(arg time.Duration) {
	verifier.workerSleepDelay = arg
}

// SetPartitionSizeMB sets the verifier’s maximum partition size in MiB.
func (verifier *Verifier) SetPartitionSizeMB(partitionSizeMB uint32) {
	verifier.partitionSizeInBytes = int64(partitionSizeMB) * 1024 * 1024
}

func (verifier *Verifier) SetRecheckMaxSizeMB(size uint) {
	verifier.recheckMaxSizeInBytes = types.ByteCount(size) * 1024 * 1024
}

func (verifier *Verifier) SetSrcNamespaces(arg []string) {
	verifier.srcNamespaces = arg
}

func (verifier *Verifier) SetDstNamespaces(arg []string) {
	verifier.dstNamespaces = arg
}

func (verifier *Verifier) SetNamespaceMap() {
	verifier.nsMap.PopulateWithNamespaces(verifier.srcNamespaces, verifier.dstNamespaces)
}

func (verifier *Verifier) SetMetaDBName(arg string) {
	verifier.metaDBName = arg
}

func (verifier *Verifier) SetDocCompareMethod(method DocCompareMethod) {
	verifier.docCompareMethod = method
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

func (verifier *Verifier) SetPprofInterval(arg string) error {
	if arg == "" {
		return nil
	}

	interval, err := time.ParseDuration(arg)
	if err != nil {
		return err
	}

	verifier.pprofInterval = interval
	return nil
}

// DocumentStats gets various stats (TODO clarify)
func DocumentStats(ctx context.Context, client *mongo.Client, namespaces []string) {

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Doc Count", "Database", "Collection"})

	for _, n := range namespaces {
		db, coll := SplitNamespace(n)
		if db != "" {
			s, _ := client.Database(db).Collection(coll).EstimatedDocumentCount(ctx)
			table.Append(
				[]string{
					humanize.Comma(s),
					db,
					coll,
				},
			)
		}
	}
	table.Render()
	fmt.Println()
}

func (verifier *Verifier) getGeneration() (generation int, lastGeneration bool) {
	verifier.mux.RLock()
	generation, lastGeneration = verifier.generation, verifier.lastGeneration
	verifier.mux.RUnlock()
	return
}

func (verifier *Verifier) getGenerationWhileLocked() (int, bool) {

	// As long as no other goroutine has locked the mux this will
	// usefully panic if the caller neglected the lock.
	wasUnlocked := verifier.mux.TryLock()
	if wasUnlocked {
		verifier.mux.Unlock()
		panic("getGenerationWhileLocked() while unlocked")
	}

	return verifier.generation, verifier.lastGeneration
}

func (verifier *Verifier) maybeAppendGlobalFilterToPredicates(predicates bson.A) bson.A {
	if len(verifier.globalFilter) == 0 {
		verifier.logger.Debug().Msg("No filter to append; globalFilter is nil")
		return predicates
	}
	verifier.logger.Debug().Str("filter", fmt.Sprintf("%v", verifier.globalFilter)).Msg("Appending filter to find query")
	return append(predicates, verifier.globalFilter)
}

func mismatchResultsToVerificationResults(mismatch *MismatchDetails, srcClientDoc, dstClientDoc bson.Raw, namespace string, id any, fieldPrefix string) (results []VerificationResult) {

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
		details := Mismatch + fmt.Sprintf(" : Document %s failed comparison on field %#q between srcClient (Type: %s) and dstClient (Type: %s)", id, fieldPrefix+field, srcClientValue.Type, dstClientValue.Type)
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

func (verifier *Verifier) ProcessVerifyTask(ctx context.Context, workerNum int, task *VerificationTask) error {
	start := time.Now()

	debugLog := verifier.logger.Debug().
		Int("workerNum", workerNum).
		Any("task", task.PrimaryKey).
		Str("namespace", task.QueryFilter.Namespace)

	task.augmentLogWithDetails(debugLog)

	debugLog.Msg("Processing document comparison task.")

	var problems []VerificationResult
	var docsCount types.DocumentCount
	var bytesCount types.ByteCount
	var err error

	if task.IsRecheck() {
		var idGroups [][]any
		idGroups, err = util.SplitArrayByBSONMaxSize(task.Ids, int(verifier.recheckMaxSizeInBytes))
		if err != nil {
			return errors.Wrapf(err, "failed to split recheck task %v document IDs", task.PrimaryKey)
		}

		for _, ids := range idGroups {
			if len(idGroups) != 1 {
				verifier.logger.Info().
					Int("workerNum", workerNum).
					Any("task", task.PrimaryKey).
					Str("namespace", task.QueryFilter.Namespace).
					Int("subsetDocs", len(ids)).
					Int("totalTaskDocs", len(task.Ids)).
					Msg("Comparing subset of recheck task’s documents.")
			}

			miniTask := clone.Clone(*task)
			miniTask.Ids = ids

			var curProblems []VerificationResult
			var curDocsCount types.DocumentCount
			var curBytesCount types.ByteCount
			curProblems, curDocsCount, curBytesCount, err = verifier.FetchAndCompareDocuments(
				ctx,
				workerNum,
				&miniTask,
			)

			if err != nil {
				break
			}

			problems = append(problems, curProblems...)
			docsCount += curDocsCount
			bytesCount += curBytesCount
		}
	} else {
		problems, docsCount, bytesCount, err = verifier.FetchAndCompareDocuments(
			ctx,
			workerNum,
			task,
		)
	}

	if err != nil {
		return errors.Wrapf(
			err,
			"worker %d failed to process document comparison task %s (namespace: %#q)",
			workerNum,
			task.PrimaryKey,
			task.QueryFilter.Namespace,
		)
	}

	task.SourceDocumentCount = docsCount
	task.SourceByteCount = bytesCount

	if len(problems) == 0 {
		task.Status = verificationTaskCompleted
	} else {
		task.Status = verificationTaskFailed
		// We know we won't change lastGeneration while verification tasks are running, so no mutex needed here.
		if verifier.lastGeneration {
			verifier.logger.Error().
				Int("workerNum", workerNum).
				Any("task", task.PrimaryKey).
				Str("namespace", task.QueryFilter.Namespace).
				Int("mismatchesCount", len(problems)).
				Msg("Document(s) mismatched, and this is the final generation.")
		} else {
			verifier.logger.Debug().
				Int("workerNum", workerNum).
				Any("task", task.PrimaryKey).
				Str("namespace", task.QueryFilter.Namespace).
				Int("mismatchesCount", len(problems)).
				Msg("Discrepancies found. Will recheck in the next generation.")

			var dataSizes []int

			// This stores all IDs for the next generation to check.
			// Its length should equal len(mismatches) + len(missingIds).
			var idsToRecheck []any

			for _, mismatch := range problems {
				idsToRecheck = append(idsToRecheck, mismatch.ID)
				dataSizes = append(dataSizes, mismatch.dataSize)
			}

			// Create a task for the next generation to recheck the
			// mismatched & missing docs.
			err := verifier.InsertFailedCompareRecheckDocs(task.QueryFilter.Namespace, idsToRecheck, dataSizes)
			if err != nil {
				return errors.Wrapf(
					err,
					"failed to enqueue %d recheck(s) of mismatched documents",
					len(idsToRecheck),
				)
			}
		}

		err = recordMismatches(
			ctx,
			verifier.metaClient.Database(verifier.metaDBName),
			task.PrimaryKey,
			problems,
		)
		if err != nil {
			return errors.Wrapf(
				err,
				"recording task %s's %d discrepancies",
				task.PrimaryKey,
				len(problems),
			)
		}
	}

	err = verifier.UpdateVerificationTask(ctx, task)

	if err != nil {
		return errors.Wrapf(
			err,
			"failed to persist task %s's new status (%#q)",
			task.PrimaryKey,
			task.Status,
		)
	}

	verifier.logger.Debug().
		Int("workerNum", workerNum).
		Any("task", task.PrimaryKey).
		Str("namespace", task.QueryFilter.Namespace).
		Int64("documentCount", int64(docsCount)).
		Str("dataSize", reportutils.FmtBytes(bytesCount)).
		Stringer("timeElapsed", time.Since(start)).
		Msg("Finished document comparison task.")

	return nil
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
		var resultMap bson.M
		if err = cursor.Decode(&resultMap); err != nil {
			verifier.logger.Error().Msgf("Error decoding chunk info for %s: %v", namespace, err)
			return
		}
		verifier.logger.Debug().Msgf(" Chunk of %s on %v, range %v to %v", namespace, resultMap["shard"],
			resultMap["min"], resultMap["max"])
	}
	if err = cursor.Err(); err != nil {
		verifier.logger.Error().Msgf("Error reading chunk info for %s: %v", namespace, err)
	}
}

func (verifier *Verifier) getShardKeyFields(
	ctx context.Context,
	namespaceAndUUID *uuidutil.NamespaceAndUUID,
) ([]string, error) {
	coll := verifier.srcClient.Database(namespaceAndUUID.DBName).
		Collection(namespaceAndUUID.CollName)

	shardKeyOpt, err := util.GetShardKey(ctx, coll)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's shard key",
			FullName(coll),
		)
	}

	shardKeyRaw, isSharded := shardKeyOpt.Get()
	if !isSharded {
		return []string{}, nil
	}

	verifier.logChunkInfo(ctx, namespaceAndUUID)

	els, err := shardKeyRaw.Elements()
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to parse %#q's shard key",
			FullName(coll),
		)
	}

	return lo.Map(
		els,
		func(el bson.RawElement, _ int) string {
			return el.Key()
		},
	), nil
}

// partitionAndInspectNamespace does a few preliminary tasks for the
// given namespace:
//  1. Devise partition boundaries.
//  2. Fetch shard keys.
//  3. Fetch the size: # of docs, and # of bytes.
func (verifier *Verifier) partitionAndInspectNamespace(ctx context.Context, namespace string) ([]*partitions.Partition, []string, types.DocumentCount, types.ByteCount, error) {
	dbName, collName := SplitNamespace(namespace)
	namespaceAndUUID, err := uuidutil.GetCollectionNamespaceAndUUID(ctx, verifier.logger,
		verifier.srcClientDatabase(dbName), collName)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	shardKeys, err := verifier.getShardKeyFields(ctx, namespaceAndUUID)
	if err != nil {
		return nil, nil, 0, 0, err
	}

	partitionList, srcDocs, srcBytes, err := partitions.PartitionCollectionWithSize(
		ctx, namespaceAndUUID, verifier.srcClient, verifier.logger, verifier.partitionSizeInBytes, verifier.globalFilter)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	// TODO: Test the empty collection (which returns no partitions)
	if len(partitionList) == 0 {
		partitionList = []*partitions.Partition{{
			Key: partitions.PartitionKey{
				SourceUUID: namespaceAndUUID.UUID,
			},
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

	return partitionList, shardKeys, srcDocs, srcBytes, nil
}

// Returns a slice of VerificationResults with the differences, and a boolean indicating whether or
// not the collection data can be safely verified.
func (verifier *Verifier) compareCollectionSpecifications(
	srcNs, dstNs string,
	srcSpecOpt, dstSpecOpt option.Option[util.CollectionSpec],
) ([]VerificationResult, bool, error) {
	srcSpec, hasSrcSpec := srcSpecOpt.Get()
	dstSpec, hasDstSpec := dstSpecOpt.Get()

	if !hasSrcSpec {
		return []VerificationResult{{
			NameSpace: srcNs,
			Cluster:   ClusterSource,
			Details:   Missing,
		}}, false, nil
	}
	if !hasDstSpec {
		return []VerificationResult{{
			NameSpace: dstNs,
			Cluster:   ClusterTarget,
			Details:   Missing,
		}}, false, nil
	}
	if srcSpec.Type != dstSpec.Type {
		return []VerificationResult{{
			NameSpace: srcNs,
			Cluster:   ClusterTarget,
			Field:     "Type",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Type, dstSpec.Type),
		}}, false, nil
		// If the types differ, the rest is not important.
	}
	var results []VerificationResult
	if srcSpec.Info.ReadOnly != dstSpec.Info.ReadOnly {
		results = append(results, VerificationResult{
			NameSpace: dstNs,
			Cluster:   ClusterTarget,
			Field:     "ReadOnly",
			Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Info.ReadOnly, dstSpec.Info.ReadOnly),
		})
	}
	if !bytes.Equal(srcSpec.Options, dstSpec.Options) {
		mismatchDetails, err := BsonUnorderedCompareRawDocumentWithDetails(srcSpec.Options, dstSpec.Options)
		if err != nil {
			return nil, false, errors.Wrapf(
				err,
				"failed to compare namespace %#q's specifications",
				srcNs,
			)
		}
		if mismatchDetails == nil {
			results = append(results, VerificationResult{
				NameSpace: dstNs,
				Cluster:   ClusterTarget,
				Field:     "Options (Field Order Only)",
				Details:   Mismatch + fmt.Sprintf(" : src: %v, dst: %v", srcSpec.Options, dstSpec.Options),
			})
		} else {
			results = append(results, mismatchResultsToVerificationResults(mismatchDetails, srcSpec.Options, dstSpec.Options, srcNs, "spec", "Options.")...)
		}
	}

	// Don't compare view data; they have no data of their own.
	canCompareData := srcSpec.Type != "view"
	// Do not compare data between capped and uncapped collections because the partitioning is different.
	canCompareData = canCompareData && srcSpec.Options.Lookup("capped").Equal(dstSpec.Options.Lookup("capped"))

	return results, canCompareData, nil
}

func (verifier *Verifier) doIndexSpecsMatch(ctx context.Context, srcSpec, dstSpec bson.Raw) (bool, error) {
	// If the byte buffers match, then we’re done.
	if bytes.Equal(srcSpec, dstSpec) {
		return true, nil
	}

	var fieldsToRemove = []string{
		// v4.4 stopped adding “ns” to index fields.
		"ns",

		// v4.2+ ignores this field.
		"background",
	}

	return util.ServerThinksTheseMatch(
		ctx,
		verifier.metaClient,
		srcSpec,
		dstSpec,
		option.Some(mongo.Pipeline{
			{{"$unset", lo.Reduce(
				fieldsToRemove,
				func(cur []string, field string, _ int) []string {
					return append(cur, "a."+field, "b."+field)
				},
				[]string{},
			)}},
		}),
	)
}

func (verifier *Verifier) ProcessCollectionVerificationTask(
	ctx context.Context,
	workerNum int,
	task *VerificationTask,
) error {
	verifier.logger.Debug().
		Int("workerNum", workerNum).
		Any("task", task.PrimaryKey).
		Str("namespace", task.QueryFilter.Namespace).
		Msg("Processing collection.")

	err := verifier.verifyMetadataAndPartitionCollection(ctx, workerNum, task)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to process collection %#q for task %s",
			task.QueryFilter.Namespace,
			task.PrimaryKey,
		)
	}

	return errors.Wrapf(
		verifier.UpdateVerificationTask(ctx, task),
		"failed to update verification task %s's status",
		task.PrimaryKey,
	)
}

func getIndexesMap(
	ctx context.Context,
	coll *mongo.Collection,
) (map[string]bson.Raw, error) {

	var specs []bson.Raw
	specsMap := map[string]bson.Raw{}

	cursor, err := coll.Indexes().List(ctx)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to read %#q’s indexes",
			FullName(coll),
		)
	}
	err = cursor.All(ctx, &specs)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to parse %#q’s indexes",
			FullName(coll),
		)
	}

	for _, spec := range specs {
		var name string
		has, err := mbson.RawLookup(spec, &name, "name")

		if err != nil {
			return nil, errors.Wrapf(
				err,
				"failed to extract %#q from %#q's index specification (%+v)",
				"name",
				FullName(coll),
				spec,
			)
		}

		if !has {
			return nil, errors.Errorf(
				"%#q has an unnamed index (%+v)",
				FullName(coll),
				spec,
			)
		}

		specsMap[name] = spec
	}

	return specsMap, nil
}

func (verifier *Verifier) verifyIndexes(
	ctx context.Context,
	srcColl, dstColl *mongo.Collection,
	srcIdIndexSpec, dstIdIndexSpec bson.Raw,
) ([]VerificationResult, error) {

	srcMap, err := getIndexesMap(ctx, srcColl)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's indexes on source",
			FullName(srcColl),
		)
	}

	if srcIdIndexSpec != nil {
		srcMap["_id"] = srcIdIndexSpec
	}

	dstMap, err := getIndexesMap(ctx, dstColl)
	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to fetch %#q's indexes on destination",
			FullName(dstColl),
		)
	}

	if dstIdIndexSpec != nil {
		dstMap["_id"] = dstIdIndexSpec
	}

	var results []VerificationResult
	srcMapUsed := map[string]bool{}

	for indexName, dstSpec := range dstMap {
		srcSpec, exists := srcMap[indexName]
		if exists {
			srcMapUsed[indexName] = true
			theyMatch, err := verifier.doIndexSpecsMatch(ctx, srcSpec, dstSpec)
			if err != nil {
				return nil, errors.Wrapf(
					err,
					"failed to check whether %#q's source & desstination %#q indexes match",
					FullName(srcColl),
					indexName,
				)
			}

			if !theyMatch {
				results = append(results, VerificationResult{
					ID:        indexName,
					Field:     "index",
					NameSpace: FullName(dstColl),
					Cluster:   ClusterTarget,
					Details:   Mismatch + fmt.Sprintf(": src: %v, dst: %v", srcSpec, dstSpec),
				})
			}
		} else {
			results = append(results, VerificationResult{
				ID:        indexName,
				Field:     "index",
				Details:   Missing,
				Cluster:   ClusterSource,
				NameSpace: FullName(srcColl),
			})
		}
	}

	// Find any index specs which existed in the source cluster but not the target cluster.
	for indexName := range srcMap {
		if !srcMapUsed[indexName] {
			results = append(results, VerificationResult{
				ID:        indexName,
				Field:     "index",
				Details:   Missing,
				Cluster:   ClusterTarget,
				NameSpace: FullName(dstColl)})
		}
	}
	return results, nil
}

func (verifier *Verifier) verifyMetadataAndPartitionCollection(
	ctx context.Context,
	workerNum int,
	task *VerificationTask,
) error {
	srcColl := verifier.srcClientCollection(task)
	dstColl := verifier.dstClientCollection(task)
	srcNs := FullName(srcColl)
	dstNs := FullName(dstColl)

	srcSpecOpt, err := util.GetCollectionSpecIfExists(ctx, srcColl)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to fetch %#q's specification on source",
			FullName(srcColl),
		)
	}

	dstSpecOpt, err := util.GetCollectionSpecIfExists(ctx, dstColl)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to fetch %#q's specification on destination",
			FullName(srcColl),
		)
	}

	insertFailedCollection := func() error {
		_, err := verifier.InsertFailedCollectionVerificationTask(ctx, srcNs)
		return errors.Wrapf(
			err,
			"failed to persist metadata mismatch for collection %#q",
			srcNs,
		)
	}

	srcSpec, hasSrcSpec := srcSpecOpt.Get()
	dstSpec, hasDstSpec := dstSpecOpt.Get()

	if !hasDstSpec {
		if !hasSrcSpec {
			verifier.logger.Info().
				Int("workerNum", workerNum).
				Str("srcNamespace", srcNs).
				Str("dstNamespace", dstNs).
				Msg("Collection not present on either cluster.")

			// This counts as success.
			task.Status = verificationTaskCompleted
			return nil
		}

		task.Status = verificationTaskFailed
		// Fall through here; comparing the collection specifications will produce the correct
		// failure output.
	}
	specificationProblems, verifyData, err := verifier.compareCollectionSpecifications(srcNs, dstNs, srcSpecOpt, dstSpecOpt)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to compare collection %#q's specifications",
			srcNs,
		)
	}
	if specificationProblems != nil {
		err := recordMismatches(
			ctx,
			verifier.verificationDatabase(),
			task.PrimaryKey,
			specificationProblems,
		)
		if err != nil {
			return errors.Wrapf(err, "recording %#q spec discrepancies", srcNs)
		}

		err = insertFailedCollection()
		if err != nil {
			return err
		}

		if !verifyData {
			task.Status = verificationTaskFailed
			return nil
		}
		task.Status = verificationTaskMetadataMismatch
	}
	if !verifyData {
		// If the metadata mismatched and we're not checking the actual data, that's a complete failure.
		if task.Status == verificationTaskMetadataMismatch {
			task.Status = verificationTaskFailed
		} else {
			task.Status = verificationTaskCompleted
		}
		return nil
	}

	indexProblems, err := verifier.verifyIndexes(ctx, srcColl, dstColl, srcSpec.IDIndex, dstSpec.IDIndex)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to compare namespace %#q's indexes",
			srcNs,
		)
	}
	if indexProblems != nil {
		if specificationProblems == nil {
			// don't insert a failed collection unless we did not insert one above
			err = insertFailedCollection()
			if err != nil {
				return err
			}
		}

		err := recordMismatches(
			ctx,
			verifier.verificationDatabase(),
			task.PrimaryKey,
			indexProblems,
		)
		if err != nil {
			return errors.Wrapf(err, "recording %#q index discrepancies", srcNs)
		}

		task.Status = verificationTaskMetadataMismatch
	}

	shardingProblems, err := verifier.verifyShardingIfNeeded(ctx, srcColl, dstColl)
	if err != nil {
		return errors.Wrapf(
			err,
			"failed to compare namespace %#q's sharding",
			srcNs,
		)
	}
	if len(shardingProblems) > 0 {
		// don't insert a failed collection unless we did not insert one above
		if len(specificationProblems)+len(indexProblems) == 0 {
			err = insertFailedCollection()
			if err != nil {
				return err
			}
		}

		err := recordMismatches(
			ctx,
			verifier.verificationDatabase(),
			task.PrimaryKey,
			shardingProblems,
		)
		if err != nil {
			return errors.Wrapf(err, "recording %#q sharding discrepancies", srcNs)
		}

		task.Status = verificationTaskMetadataMismatch
	}

	// We’ve confirmed that the collection metadata (including indices)
	// matches between soruce & destination. Now we can partition the collection.

	if task.Generation == 0 {
		var partitionsCount int
		var docsCount types.DocumentCount
		var bytesCount types.ByteCount

		if verifier.srcHasSampleRate() {
			var err error
			partitionsCount, docsCount, bytesCount, err = verifier.createPartitionTasksWithSampleRate(ctx, task)
			if err != nil {
				return errors.Wrapf(err, "partitioning %#q via $sampleRate", srcNs)
			}
		} else {
			verifier.logger.Warn().
				Msg("Source MongoDB version lacks $sampleRate. Using legacy partitioning logic. This may cause imbalanced partitions, which will impede performance.")

			var partitions []*partitions.Partition
			var shardKeys []string

			partitions, shardKeys, docsCount, bytesCount, err = verifier.partitionAndInspectNamespace(ctx, srcNs)
			if err != nil {
				return errors.Wrapf(err, "partitioning %#q via $sample", srcNs)
			}

			partitionsCount = len(partitions)

			for _, partition := range partitions {
				_, err := verifier.InsertPartitionVerificationTask(ctx, partition, shardKeys, dstNs)
				if err != nil {
					return errors.Wrapf(
						err,
						"failed to insert a partition task for namespace %#q",
						srcNs,
					)
				}
			}
		}

		verifier.logger.Debug().
			Int("workerNum", workerNum).
			Str("namespace", srcNs).
			Int("partitionsCount", partitionsCount).
			Msg("Divided collection into partitions.")

		task.SourceDocumentCount = docsCount
		task.SourceByteCount = bytesCount
	}

	if task.Status == verificationTaskProcessing {
		task.Status = verificationTaskCompleted
	}

	return nil
}

func (verifier *Verifier) GetVerificationStatus(ctx context.Context) (*VerificationStatus, error) {
	taskCollection := verifier.verificationTaskCollection()
	generation, _ := verifier.getGeneration()

	var results []bson.Raw

	err := retry.New().WithCallback(
		func(ctx context.Context, _ *retry.FuncInfo) error {
			cursor, err := taskCollection.Aggregate(
				ctx,
				[]bson.M{
					{
						"$match": bson.M{
							"type":       bson.M{"$ne": "primary"},
							"generation": generation,
						},
					},
					{
						"$group": bson.M{
							"_id":   "$status",
							"count": bson.M{"$sum": 1},
						},
					},
				},
			)
			if err != nil {
				return err
			}

			return cursor.All(ctx, &results)
		},
		"counting generation %d's (non-primary) tasks by status",
		generation,
	).Run(ctx, verifier.logger)

	if err != nil {
		return nil, errors.Wrapf(
			err,
			"failed to count generation %d's tasks by status",
			generation,
		)
	}

	verificationStatus := VerificationStatus{}

	for _, result := range results {
		status := result.Lookup("_id").String()
		// Status is returned with quotes around it so remove those
		status = status[1 : len(status)-1]
		count := int(result.Lookup("count").Int32())
		verificationStatus.TotalTasks += int(count)
		switch verificationTaskStatus(status) {
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
		default:
			verifier.logger.Info().Msgf("Unknown task status %s", status)
		}
	}

	return &verificationStatus, nil
}

func (verifier *Verifier) doIfForceReadConcernMajority(f func()) {
	switch verifier.readConcernSetting {
	case ReadConcernMajority:
		f()
	case ReadConcernIgnore:
		// Do nothing.
	default:
		// invariant
		panic("Unknown read concern setting: " + verifier.readConcernSetting)
	}
}

func (verifier *Verifier) verificationDatabase() *mongo.Database {
	db := verifier.metaClient.Database(verifier.metaDBName)
	if db.WriteConcern().W != "majority" {
		verifier.logger.Fatal().Msgf("Verification metadata is not using write concern majority: %+v", db.WriteConcern())
	}

	verifier.doIfForceReadConcernMajority(func() {
		if db.ReadConcern().Level != "majority" {
			verifier.logger.Fatal().Msgf("Verification metadata is not using read concern majority: %+v", db.ReadConcern())
		}
	})

	return db
}

func (verifier *Verifier) verificationTaskCollection() *mongo.Collection {
	return verifier.verificationDatabase().Collection(verificationTasksCollection)
}

func (verifier *Verifier) srcClientDatabase(dbName string) *mongo.Database {
	db := verifier.srcClient.Database(dbName)
	// No need to check the write concern because we do not write to the source database.
	verifier.doIfForceReadConcernMajority(func() {
		if db.ReadConcern().Level != "majority" {
			verifier.logger.Fatal().Msgf("Source client is not using read concern majority: %+v", db.ReadConcern())
		}
	})
	return db
}

func (verifier *Verifier) dstClientDatabase(dbName string) *mongo.Database {
	db := verifier.dstClient.Database(dbName)
	// No need to check the write concern because we do not write to the target database.
	verifier.doIfForceReadConcernMajority(func() {
		if db.ReadConcern().Level != "majority" {
			verifier.logger.Fatal().Msgf("Source client is not using read concern majority: %+v", db.ReadConcern())
		}
	})
	return db
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
	status, err := verifier.GetVerificationStatus(ctx)
	if err != nil {
		return Progress{Error: err}, err
	}
	return Progress{
		Phase:      verifier.phase,
		Generation: verifier.generation,
		Status:     status,
	}, nil
}

// Returned boolean indicates that namespaces are cached, and
// whatever needs them can proceed.
func (verifier *Verifier) ensureNamespaces(ctx context.Context) bool {

	// cache namespace
	if len(verifier.srcNamespaces) == 0 {
		namespaces, err := verifier.getNamespaces(ctx, SrcNamespaceField)
		if err != nil {
			verifier.logger.Err(err).Msgf("Failed to learn source namespaces")
			return false
		}

		verifier.srcNamespaces = namespaces

		// if still no namespace, nothing to print!
		if len(verifier.srcNamespaces) == 0 {
			verifier.logger.Info().Msg("Source contains no namespaces.")
			return false
		}
	}

	if len(verifier.dstNamespaces) == 0 {
		namespaces, err := verifier.getNamespaces(ctx, DstNamespaceField)

		if err != nil {
			verifier.logger.Err(err).Msgf("Failed to learn destination namespaces")
			return false
		}

		verifier.dstNamespaces = namespaces

		if len(verifier.dstNamespaces) == 0 {
			verifier.dstNamespaces = verifier.srcNamespaces
		}
	}

	return true
}

const (
	dividerChar  = "\u2501" // horizontal dash
	dividerWidth = 76
)

func startReport() *strings.Builder {
	strBuilder := &strings.Builder{}

	timestampAndSpace := time.Now().Format(timeFormat) + " "
	divider := strings.Repeat(dividerChar, dividerWidth-len(timestampAndSpace))

	fmt.Fprintf(
		strBuilder,
		"\n%s%s\n\n", timestampAndSpace, divider,
	)

	return strBuilder
}

func (verifier *Verifier) PrintVerificationSummary(ctx context.Context, genstatus GenerationStatus) {
	if !verifier.ensureNamespaces(ctx) {
		return
	}

	strBuilder := startReport()

	generation, _ := verifier.getGeneration()

	var header string
	switch genstatus {
	case Gen0MetadataAnalysisComplete:
		header = fmt.Sprintf("Metadata Analysis Complete (Generation #%d)", generation)
	case GenerationInProgress:
		header = fmt.Sprintf("Progress Report (Generation #%d)", generation)
	case GenerationComplete:
		header = fmt.Sprintf("End of Generation #%d", generation)
	default:
		panic("Bad generation status: " + genstatus)
	}

	strBuilder.WriteString(header + "\n\n")

	reportGenStartTime := time.Now()
	elapsedSinceGenStart := reportGenStartTime.Sub(verifier.generationStartTime)

	fmt.Fprintf(
		strBuilder,
		"Generation time elapsed: %s\n",
		reportutils.DurationToHMS(elapsedSinceGenStart),
	)

	metadataMismatches, anyCollsIncomplete, err := verifier.reportCollectionMetadataMismatches(ctx, strBuilder)
	if err != nil {
		verifier.logger.Err(err).Msgf("Failed to report collection metadata mismatches")
		return
	}

	var hasTasks bool
	switch genstatus {
	case Gen0MetadataAnalysisComplete:
		fallthrough
	case GenerationInProgress:
		hasTasks, err = verifier.printNamespaceStatistics(ctx, strBuilder, reportGenStartTime)
	case GenerationComplete:
		hasTasks, err = verifier.printEndOfGenerationStatistics(ctx, strBuilder, reportGenStartTime)
	default:
		panic("Bad generation status: " + genstatus)
	}

	if err != nil {
		verifier.logger.Err(err).Msgf("Failed to report per-namespace statistics")
		return
	}

	verifier.printChangeEventStatistics(strBuilder, reportGenStartTime)

	// Only print the worker status table if debug logging is enabled.
	if verifier.logger.Debug().Enabled() {
		switch genstatus {
		case Gen0MetadataAnalysisComplete, GenerationInProgress:
			verifier.printWorkerStatus(strBuilder, reportGenStartTime)
		}
	}

	var statusLine string

	if hasTasks {
		docMismatches, anyPartitionsIncomplete, err := verifier.reportDocumentMismatches(ctx, strBuilder)
		if err != nil {
			verifier.logger.Err(err).Msgf("Failed to report document mismatches")
			return
		}

		if metadataMismatches || docMismatches {
			verifier.printMismatchInvestigationNotes(strBuilder)

			statusLine = fmt.Sprintf(notOkSymbol + " Mismatches found.")
		} else if anyCollsIncomplete || anyPartitionsIncomplete {
			statusLine = fmt.Sprintf(infoSymbol + " No mismatches found yet, but verification is still in progress.")
		} else {
			statusLine = fmt.Sprintf(okSymbol + " No mismatches found. Source & destination completely match!")
		}
	} else {
		switch genstatus {
		case Gen0MetadataAnalysisComplete:
			fallthrough
		case GenerationInProgress:
			statusLine = "This generation has nothing to compare."
		case GenerationComplete:
			statusLine = "This generation had nothing to compare."
		default:
			panic("Bad generation status: " + genstatus)
		}

		statusLine = okSymbol + " " + statusLine
	}

	strBuilder.WriteString("\n" + statusLine + "\n")

	elapsed := time.Since(reportGenStartTime)
	if elapsed > progressReportTimeWarnThreshold {
		verifier.logger.Warn().
			Stringer("elapsed", elapsed).
			Msg("Report generation took longer than expected. The metadata database may be under excess load.")
	}

	verifier.writeStringBuilder(strBuilder)
}

func (verifier *Verifier) writeStringBuilder(builder *strings.Builder) {
	_, err := verifier.writer.Write([]byte(builder.String()))
	if err != nil {
		panic("Failed to write to string builder: " + err.Error())
	}
}

func (verifier *Verifier) getNamespaces(ctx context.Context, fieldName string) ([]string, error) {
	var namespaces []string
	ret, err := verifier.verificationTaskCollection().Distinct(ctx, fieldName, bson.D{})
	if err != nil {
		return nil, err
	}
	for _, v := range ret {
		namespaces = append(namespaces, v.(string))
	}
	return namespaces, nil
}
