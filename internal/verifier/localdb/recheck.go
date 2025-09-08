package localdb

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/pkg/errors"
	"github.com/samber/mo"
	"go.etcd.io/bbolt"
	"go.mongodb.org/mongo-driver/bson"
)

// ----------------------------------------------------------------------------
// Rechecks are stored like this:
//
// recheck-0-dbName.collName: {
//   $docIdHash: bson(recheckInternal), bson(recheckInternal), ...
// }
// recheck-meta-0: {
//   count: 123,
// }
//
// Ideally we could let the keys of `recheck-0-dbName.collName` be the
// document ID, but bbolt limits bucket keys to 32 KiB.
// ----------------------------------------------------------------------------

const (
	recheckBucketPrefix     = "recheck-"
	recheckMetaBucketPrefix = "recheck-meta-"
	countKey                = "count"
)

type recheckInternal struct {
	DocID   bson.RawValue
	DocSize int
}

func (ldb *LocalDB) ClearAllRechecksForGeneration(generation int) error {
	bucketPrefix := getRecheckBucketPrefixForGeneration(generation)

	return ldb.db.Update(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			if bytes.HasPrefix(name, []byte(bucketPrefix)) {
				if err := tx.DeleteBucket(name); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func getRecheckBucketPrefixForGeneration(generation int) string {
	return recheckBucketPrefix + strconv.Itoa(generation) + "-"
}

func getRecheckMetaBucketForGeneration(generation int) string {
	return recheckMetaBucketPrefix + strconv.Itoa(generation)
}

type Recheck struct {
	DB, Coll string
	DocID    bson.RawValue
	Size     types.ByteCount
}

func (ldb *LocalDB) GetRechecksCount(generation int) (uint64, error) {
	var count uint64

	err := ldb.db.View(func(tx *bbolt.Tx) error {
		var err error
		count, err = getRechecksCountInTxn(tx, generation)
		return err
	})

	return count, err
}

func getRechecksCountInTxn(tx *bbolt.Tx, generation int) (uint64, error) {
	var count uint64

	bucket := getRecheckMetaBucketForGeneration(generation)

	metaBucket := tx.Bucket([]byte(bucket))
	if metaBucket == nil {
		return 0, nil
	}

	var err error
	countBytes := metaBucket.Get([]byte(countKey))
	if countBytes == nil {
		return 0, nil
	}

	count, err = parseUint(countBytes)
	if err != nil {
		return 0, errors.Wrapf(err, "parsing rechecks count %v", countBytes)
	}

	return count, err
}

func (ldb *LocalDB) GetRecheckReader(ctx context.Context, generation int) <-chan mo.Result[Recheck] {
	retChan := make(chan mo.Result[Recheck])

	bucketPrefix := getRecheckBucketPrefixForGeneration(generation)

	var foundRechecks uint64

	go func() {
		defer close(retChan)

		err := ldb.db.View(func(tx *bbolt.Tx) error {
			expectedRechecks, err := getRechecksCountInTxn(tx, generation)
			if err != nil {
				return errors.Wrapf(err, "reading count of generation %d’s rechecks", generation)
			}

			err = tx.ForEach(func(name []byte, bucket *bbolt.Bucket) error {
				if !bytes.HasPrefix(name, []byte(bucketPrefix)) {
					return nil
				}

				ns := string(bytes.TrimPrefix(name, []byte(bucketPrefix)))
				db, coll, foundDot := strings.Cut(ns, ".")
				if !foundDot {
					return fmt.Errorf("found invalid recheck bucket %#q (no dot)", string(name))
				}

				return bucket.ForEach(func(_, docs []byte) error {
					iterator := mbson.NewIterator(bytes.NewReader(docs))

					for {
						docOpt, err := iterator.Next()
						if err != nil {
							return errors.Wrap(err, "reading next BSON doc")
						}

						doc, hasDoc := docOpt.Get()
						if !hasDoc {
							break
						}

						var ri recheckInternal
						err = bson.Unmarshal(doc, &ri)
						if err != nil {
							return errors.Wrapf(err, "unmarshaling recheck (%v)", doc)
						}

						recheck := Recheck{
							DB:    db,
							Coll:  coll,
							DocID: ri.DocID,
							Size:  types.ByteCount(ri.DocSize),
						}

						select {
						case <-ctx.Done():
							return ctx.Err()
						case retChan <- mo.Ok(recheck):
							foundRechecks++
						}
					}

					return nil
				})
			})
			if err != nil {
				return err
			}

			if foundRechecks != expectedRechecks {
				return fmt.Errorf(
					"internal corruption: expected rechecks (%d) mismatches found rechecks (%d)",
					expectedRechecks,
					foundRechecks,
				)
			}

			return nil
		})

		if err != nil {
			select {
			case <-ctx.Done():
				ldb.log.Warn().
					Err(err).
					Msg("Failed to read rechecks.")

				return
			case retChan <- mo.Err[Recheck](err):
			}
		}
	}()

	return retChan
}

func (ldb *LocalDB) InsertRechecks(
	generation int,
	dbNames []string,
	collNames []string,
	documentIDs []any,
	dataSizes []int,
) error {
	bucketPrefix := getRecheckBucketPrefixForGeneration(generation)

	return errors.Wrapf(
		ldb.db.Update(func(tx *bbolt.Tx) error {
			bucketCache := map[string]*bbolt.Bucket{}

			for i, dbName := range dbNames {
				bsonType, bsonIDVal, err := bson.MarshalValue(documentIDs[i])
				if err != nil {
					return errors.Wrapf(err, "marshaling document ID (%v)", documentIDs[i])
				}

				collName := collNames[i]

				namespace := dbName + "." + collName

				bucket, ok := bucketCache[namespace]
				if !ok {
					bucketName := bucketPrefix + namespace

					var err error
					bucket, err = getBucket(tx, bucketName)
					if err != nil {
						return errors.Wrapf(err, "getting bucket %#q", namespace)
					}

					bucketCache[namespace] = bucket
				}

				docIDRaw := bson.RawValue{
					Type:  bsonType,
					Value: bsonIDVal,
				}
				sum := hashRawValue(docIDRaw)

				existingRechecks := bucket.Get(sum)
				docExists, err := internalRechecksHaveDocID(existingRechecks, docIDRaw)
				if err != nil {
					return errors.Wrapf(
						err,
						"checking if doc already enqueued for recheck",
					)
				}

				if !docExists {
					newRecheck := recheckInternal{
						DocID:   docIDRaw,
						DocSize: dataSizes[i],
					}

					newRecheckRaw, err := bson.Marshal(newRecheck)
					if err != nil {
						return errors.Wrapf(
							err,
							"marshaling internal recheck",
						)
					}

					buf := append(existingRechecks, newRecheckRaw...)

					err = bucket.Put(sum, buf)
					if err != nil {
						return errors.Wrapf(
							err,
							"persisting recheck for %#q",
							namespace,
						)
					}
				}
			}

			metaBucketName := getRecheckMetaBucketForGeneration(generation)
			metaBucket, err := getBucket(tx, metaBucketName)
			if err != nil {
				return errors.Wrapf(err, "getting bucket %#q", metaBucketName)
			}

			var curCount uint64
			countBytes := metaBucket.Get([]byte(countKey))
			if countBytes != nil {
				curCount, err = parseUint(countBytes)
				if err != nil {
					return errors.Wrapf(err, "parsing rechecks count %v", countBytes)
				}
			}

			countBytes = formatUint(uint64(len(dataSizes)) + curCount)
			err = metaBucket.Put([]byte(countKey), countBytes)
			if err != nil {
				return errors.Wrapf(err, "persisting rechecks count %v", countBytes)
			}

			return nil
		}),
		"persisting %d recheck(s)",
		len(documentIDs),
	)
}

func hashRawValue(rv bson.RawValue) []byte {
	idHash := fnv.New128a()
	_, _ = idHash.Write([]byte{byte(rv.Type)})
	_, _ = idHash.Write(rv.Value)
	return idHash.Sum(nil)
}

func internalRechecksHaveDocID(rechecks []byte, docID bson.RawValue) (bool, error) {
	iterator := mbson.NewIterator(bytes.NewReader(rechecks))

	for {
		docOpt, err := iterator.Next()
		if err != nil {
			return false, errors.Wrap(err, "iterating enqueued docs")
		}

		doc, hasDoc := docOpt.Get()
		if !hasDoc {
			break
		}

		var ri recheckInternal
		if err := bson.Unmarshal(doc, &ri); err != nil {
			return false, errors.Wrapf(err, "unmarshaling (%v)", doc)
		}

		if docID.Equal(ri.DocID) {
			return true, nil
		}
	}

	return false, nil
}
