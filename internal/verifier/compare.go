package verifier

import (
	"bytes"
	"context"
	"reflect"
	"slices"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/sync/errgroup"
)

func (verifier *Verifier) FetchAndCompareDocuments(
	givenCtx context.Context,
	task *VerificationTask,
) (
	[]VerificationResult,
	types.DocumentCount,
	types.ByteCount,
	error,
) {
	errGroup, ctx := errgroup.WithContext(givenCtx)

	srcChannel, dstChannel := verifier.getFetcherChannels(ctx, errGroup, task)

	results := []VerificationResult{}
	var docCount types.DocumentCount
	var byteCount types.ByteCount

	errGroup.Go(func() error {
		var err error
		results, docCount, byteCount, err = verifier.compareDocsFromChannels(
			ctx,
			task,
			srcChannel,
			dstChannel,
		)

		return err
	})

	err := errGroup.Wait()

	return results, docCount, byteCount, err
}

func (verifier *Verifier) compareDocsFromChannels(
	ctx context.Context,
	task *VerificationTask,
	srcChannel, dstChannel <-chan bson.Raw,
) (
	[]VerificationResult,
	types.DocumentCount,
	types.ByteCount,
	error,
) {
	results := []VerificationResult{}
	var docCount types.DocumentCount
	var byteCount types.ByteCount

	mapKeyFieldNames := make([]string, 1+len(task.QueryFilter.ShardKeys))
	mapKeyFieldNames[0] = "_id"
	copy(mapKeyFieldNames[1:], task.QueryFilter.ShardKeys)

	namespace := task.QueryFilter.Namespace

	srcCache := map[string]bson.Raw{}
	dstCache := map[string]bson.Raw{}

	sCases := []reflect.SelectCase{
		{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ctx.Done()),
		},
		{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(srcChannel),
		},
		{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(dstChannel),
		},
	}

	sDetails := []struct {
		OurMap   map[string]bson.Raw
		TheirMap map[string]bson.Raw
		IsSrc    bool
	}{
		{}, // ctx.Done()
		{
			IsSrc:    true,
			OurMap:   srcCache,
			TheirMap: dstCache,
		},
		{
			OurMap:   dstCache,
			TheirMap: srcCache,
		},
	}

	for len(sCases) > 1 {
		chosen, recv, alive := reflect.Select(sCases)

		if chosen == 0 {
			return nil, 0, 0, ctx.Err()
		}

		if !alive {
			sCases = slices.Delete(sCases, chosen, 1+chosen)
			sDetails = slices.Delete(sDetails, chosen, 1+chosen)
			continue
		}

		doc := (recv.Interface()).(bson.Raw)

		details := sDetails[chosen]

		if details.IsSrc {
			docCount++
			byteCount += types.ByteCount(len(doc))
		}

		mapKey := getMapKey(doc, mapKeyFieldNames)

		if theirDoc, exists := details.TheirMap[mapKey]; exists {
			var srcDoc, dstDoc bson.Raw
			if details.IsSrc {
				srcDoc = doc
				dstDoc = theirDoc
			} else {
				srcDoc = theirDoc
				dstDoc = doc
			}

			delete(details.TheirMap, mapKey)

			mismatches, err := verifier.compareOneDocument(srcDoc, dstDoc, namespace)
			if err != nil {
				return nil, 0, 0, errors.Wrap(err, "failed to compare documents")
			}

			if len(mismatches) > 0 {
				results = append(results, mismatches...)
			}
		} else {
			details.OurMap[mapKey] = doc
		}
	}

	results = slices.Grow(results, len(srcCache)+len(dstCache))

	for _, doc := range srcCache {
		results = append(
			results,
			VerificationResult{
				ID:        doc.Lookup("_id"),
				Details:   Missing,
				Cluster:   ClusterTarget,
				NameSpace: namespace,
				dataSize:  len(doc),
			},
		)
	}

	for _, doc := range dstCache {
		results = append(
			results,
			VerificationResult{
				ID:        doc.Lookup("_id"),
				Details:   Missing,
				Cluster:   ClusterSource,
				NameSpace: namespace,
				dataSize:  len(doc),
			},
		)
	}

	return results, docCount, byteCount, nil
}

func (verifier *Verifier) getFetcherChannels(
	ctx context.Context,
	errGroup *errgroup.Group,
	task *VerificationTask,
) (<-chan bson.Raw, <-chan bson.Raw) {
	srcChannel := make(chan bson.Raw)
	dstChannel := make(chan bson.Raw)

	errGroup.Go(func() error {
		cursor, err := verifier.getDocumentsCursor(
			ctx,
			verifier.srcClientCollection(task),
			verifier.srcBuildInfo,
			verifier.srcStartAtTs,
			task,
		)

		if err == nil {
			err = iterateCursorToChannel(ctx, cursor, srcChannel)
		}

		return err
	})

	errGroup.Go(func() error {
		cursor, err := verifier.getDocumentsCursor(
			ctx,
			verifier.dstClientCollection(task),
			verifier.dstBuildInfo,
			nil, //startAtTs
			task,
		)

		if err == nil {
			err = iterateCursorToChannel(ctx, cursor, dstChannel)
		}

		return err
	})

	return srcChannel, dstChannel
}

func iterateCursorToChannel(ctx context.Context, cursor *mongo.Cursor, writer chan<- bson.Raw) error {
	for cursor.Next(ctx) {
		writer <- slices.Clone(cursor.Current)
	}

	close(writer)

	return errors.Wrap(cursor.Err(), "failed to iterate cursor")
}

func getMapKey(doc bson.Raw, fieldNames []string) string {
	var keyBuffer bytes.Buffer
	for _, keyName := range fieldNames {
		value := doc.Lookup(keyName)
		keyBuffer.Grow(1 + len(value.Value))
		keyBuffer.WriteByte(byte(value.Type))
		keyBuffer.Write(value.Value)
	}

	return keyBuffer.String()
}
