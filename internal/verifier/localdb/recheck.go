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
	"github.com/dgraph-io/badger/v4"
	"github.com/pkg/errors"
	"github.com/samber/mo"
	"go.mongodb.org/mongo-driver/bson"
)

const (
	recheckBucketPrefix   = "recheck-"
	recheckCountKeyPrefix = "recheckcount-"
)

type recheckInternal struct {
	DocID   bson.RawValue
	DocSize int
}

// ClearAllRechecksForGeneration removes all rechecks for the given generation.
func (ldb *LocalDB) ClearAllRechecksForGeneration(generation int) error {
	bucketPrefix := getRecheckBucketPrefixForGeneration(generation)

	return ldb.db.Update(func(tx *badger.Txn) error {
		iteratorOpts := badger.DefaultIteratorOptions
		iteratorOpts.Prefix = []byte(bucketPrefix)
		iteratorOpts.PrefetchValues = false

		iter := tx.NewIterator(iteratorOpts)
		defer iter.Close()

		for iter.Rewind(); iter.Valid(); iter.Next() {
			if err := tx.Delete(iter.Item().Key()); err != nil {
				return errors.Wrapf(
					err,
					"deleting key %#q",
					string(iter.Item().Key()),
				)
			}
		}

		err := tx.Delete([]byte(getRecheckCountKeyForGeneration(generation)))
		if err != nil {
			return errors.Wrapf(
				err,
				"deleting generation %d’s recheck count",
				generation,
			)
		}

		return nil
	})
}

func getRecheckBucketPrefixForGeneration(generation int) string {
	return recheckBucketPrefix + strconv.Itoa(generation) + "-"
}

func getRecheckCountKeyForGeneration(generation int) string {
	return recheckCountKeyPrefix + strconv.Itoa(generation)
}

// Recheck represents a single enqueued recheck. Note that, because this
// only stores a document ID rather than a document key, it could actually
// refer to multiple documents (i.e., duplicated _id across shards).
type Recheck struct {
	DB, Coll string
	DocID    bson.RawValue
	Size     types.ByteCount
}

// GetRechecksCount returns the number of enqueued rechecks for the given
// generation.
func (ldb *LocalDB) GetRechecksCount(generation int) (uint64, error) {
	var count uint64

	err := ldb.db.View(func(tx *badger.Txn) error {
		var err error
		count, err = getRechecksCountInTxn(tx, generation)
		return err
	})

	return count, err
}

func getRechecksCountInTxn(tx *badger.Txn, generation int) (uint64, error) {
	var count uint64

	key := getRecheckCountKeyForGeneration(generation)

	item, err := tx.Get([]byte(key))
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return 0, nil
		}

		return 0, errors.Wrapf(err, "getting gen %d recheck count", generation)
	}

	countBytes, err := item.ValueCopy(nil)
	if err != nil {
		return 0, errors.Wrap(err, "copying value")
	}

	count, err = parseUint(countBytes)
	if err != nil {
		return 0, errors.Wrapf(err, "parsing rechecks count %v", countBytes)
	}

	return count, err
}

// GetRecheckReader returns a channel from which the caller can read
// Rechecks. If the context is canceled, the channel will be closed without an
// error. If any other error condition appears while reading the rechecks,
// that error will go into the channel, and the channel will be closed.
//
// The rechecks will be returned sorted by namespace; within a namespace,
// however, no sort order is defined.
func (ldb *LocalDB) GetRecheckReader(ctx context.Context, generation int) <-chan mo.Result[Recheck] {
	retChan := make(chan mo.Result[Recheck])

	bucketPrefix := getRecheckBucketPrefixForGeneration(generation)

	var foundRechecks uint64

	go func() {
		defer close(retChan)

		var canceled bool

		err := ldb.db.View(func(txn *badger.Txn) error {
			expectedRechecks, err := getRechecksCountInTxn(txn, generation)
			if err != nil {
				return errors.Wrapf(err, "reading count of generation %d’s rechecks", generation)
			}

			iteratorOpts := badger.DefaultIteratorOptions
			iteratorOpts.Prefix = []byte(bucketPrefix)
			iter := txn.NewIterator(iteratorOpts)
			defer iter.Close()

			for iter.Rewind(); iter.Valid(); iter.Next() {
				afterPrefix := strings.TrimPrefix(string(iter.Item().KeyCopy(nil)), bucketPrefix)
				ns, _, err := parseKeyMinusPrefix(afterPrefix)
				if err != nil {
					return errors.Wrapf(err, "parsing key after prefix %#q", afterPrefix)
				}

				db, coll, foundDot := strings.Cut(ns, ".")
				if !foundDot {
					return fmt.Errorf("recheck namespace %#q lacks dot", string(ns))
				}

				err = iter.Item().Value(func(val []byte) error {
					iterator := mbson.NewIterator(bytes.NewReader(val))

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
							canceled = true

							ldb.log.Debug().
								Err(err).
								Msg("Reading of rechecks was canceled.")

							return ctx.Err()
						case retChan <- mo.Ok(recheck):
							foundRechecks++
						}
					}

					return nil
				})

				if err != nil {
					return err
				}
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

		if err != nil && !canceled {
			select {
			case <-ctx.Done():
			case retChan <- mo.Err[Recheck](err):
			}
		}
	}()

	return retChan
}

// InsertRechecks enqueues rechecks. The given slices must be of the same length.
func (ldb *LocalDB) InsertRechecks(
	generation int,
	dbNames []string,
	collNames []string,
	documentIDs []any,
	dataSizes []int,
) error {
	bucketPrefix := getRecheckBucketPrefixForGeneration(generation)

	return errors.Wrapf(
		ldb.db.Update(func(tx *badger.Txn) error {
			var addedRechecks uint64

			for i, dbName := range dbNames {
				collName := collNames[i]

				namespace := dbName + "." + collName

				bsonType, bsonIDVal, err := bson.MarshalValue(documentIDs[i])
				if err != nil {
					return errors.Wrapf(err, "marshaling %#q document ID (%v)", namespace, documentIDs[i])
				}

				docIDRaw := bson.RawValue{
					Type:  bsonType,
					Value: bsonIDVal,
				}

				sum := hashRawValue(docIDRaw)

				newKey := strings.Join(
					[]string{
						bucketPrefix,
						strconv.Itoa(len(namespace)),
						"-",
						namespace,
						string(sum),
					},
					"",
				)

				var existingRechecks []byte
				oldRechecksItem, err := tx.Get([]byte(newKey))
				if err != nil {
					if !errors.Is(err, badger.ErrKeyNotFound) {
						return errors.Wrapf(err, "fetching item %#q", newKey)
					}
				} else {
					existingRechecks, err = oldRechecksItem.ValueCopy(nil)
					if err != nil {
						return errors.Wrapf(err, "copying value %#q", newKey)
					}
				}

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

					err = tx.Set([]byte(newKey), buf)
					if err != nil {
						return errors.Wrapf(
							err,
							"persisting recheck for %#q",
							namespace,
						)
					}

					addedRechecks++
				}
			}

			var curCount uint64
			curCount, err := getRechecksCountInTxn(tx, generation)
			if err != nil {
				return errors.Wrapf(err, "reading gen %d’s rechecks count", generation)
			}
			countBytes := formatUint(addedRechecks + curCount)

			metaBucketName := getRecheckCountKeyForGeneration(generation)

			err = tx.Set([]byte(metaBucketName), countBytes)
			if err != nil {
				return errors.Wrapf(err, "persisting rechecks count %v", countBytes)
			}

			return nil
		}),
		"persisting %d recheck(s)",
		len(documentIDs),
	)
}

// returns ns and doc ID hash
func parseKeyMinusPrefix(in string) (string, []byte, error) {
	beforeDash, afterDash, found := strings.Cut(in, "-")
	if !found {
		return "", nil, fmt.Errorf("invalid recheck key part: %#q", in)
	}

	nsLen, err := strconv.Atoi(beforeDash)
	if err != nil {
		return "", nil, errors.Wrapf(err, "parsing ns len %#q", nsLen)
	}

	return afterDash[:nsLen], []byte(afterDash[nsLen:]), nil
}

func hashRawValue(rv bson.RawValue) []byte {
	idHash := fnv.New64a()
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
