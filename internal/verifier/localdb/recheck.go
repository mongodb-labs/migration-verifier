package localdb

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/pkg/errors"
	"github.com/samber/mo"
	"go.etcd.io/bbolt"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

const (
	recheckBucketPrefix = "recheck-"
)

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

type Recheck struct {
	DB, Coll string
	DocID    bson.RawValue
	Size     types.ByteCount
}

func (ldb *LocalDB) CountRechecks(generation int) (int, error) {
	bucketPrefix := getRecheckBucketPrefixForGeneration(generation)

	count := 0

	err := ldb.db.View(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, bucket *bbolt.Bucket) error {
			if !bytes.HasPrefix(name, []byte(bucketPrefix)) {
				return nil
			}

			count += bucket.Stats().KeyN
			return nil
		})
	})

	if err != nil {
		return 0, err
	}

	return count, nil
}

func (ldb *LocalDB) GetRecheckReader(ctx context.Context, generation int) <-chan mo.Result[Recheck] {
	retChan := make(chan mo.Result[Recheck])

	bucketPrefix := getRecheckBucketPrefixForGeneration(generation)

	go func() {
		defer close(retChan)

		err := ldb.db.View(func(tx *bbolt.Tx) error {
			return tx.ForEach(func(name []byte, bucket *bbolt.Bucket) error {
				if !bytes.HasPrefix(name, []byte(bucketPrefix)) {
					return nil
				}

				ns := string(bytes.TrimPrefix(name, []byte(bucketPrefix)))
				db, coll, foundDot := strings.Cut(ns, ".")
				if !foundDot {
					return fmt.Errorf("found invalid recheck bucket %#q (no dot)", string(name))
				}

				return bucket.ForEach(func(k, v []byte) error {
					docID := bson.RawValue{
						Type:  bsontype.Type(k[0]),
						Value: k[1:],
					}

					size, err := parseUint(v)
					if err != nil {
						ldb.log.Warn().
							Any("docID", k).
							Bytes("size", v).
							Msg("Failed to parse %#q recheck.")
					}

					recheck := Recheck{
						DB:    db,
						Coll:  coll,
						DocID: docID,
						Size:  types.ByteCount(size),
					}

					select {
					case <-ctx.Done():
						return ctx.Err()
					case retChan <- mo.Ok(recheck):
					}

					return nil
				})
			})
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

				bsonID := append([]byte{byte(bsonType)}, bsonIDVal...)

				err = bucket.Put(bsonID, formatUint(uint64(dataSizes[i])))
				if err != nil {
					return errors.Wrapf(
						err,
						"persisting recheck for %#q",
						namespace,
					)
				}
			}

			return nil
		}),
		"persisting %d recheck(s)",
		len(documentIDs),
	)
}
