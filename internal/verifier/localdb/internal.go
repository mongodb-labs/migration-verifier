package localdb

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/pkg/errors"
	"go.etcd.io/bbolt"
	"golang.org/x/exp/constraints"
)

const (
	schemaVersionKey = "formatVersion"
	schemaVersion    = uint16(3)

	metadataBucketName = "metadata"
)

func verifySchemaVersion(db *bbolt.DB) error {
	metadataVersionBytes := formatUint(schemaVersion)

	return db.Update(func(tx *bbolt.Tx) error {
		bucket, err := getMetadataBucket(tx)
		if err != nil {
			return err
		}

		versionBytes := bucket.Get([]byte(schemaVersionKey))

		if versionBytes != nil {
			if bytes.Equal(versionBytes, metadataVersionBytes) {
				return nil
			}
			foundVersion, err := parseUint(versionBytes)
			if err != nil {
				return fmt.Errorf("found metadata version %d, but %d is required; is this local DB from a prior verifier version?", foundVersion, schemaVersion)
			}

			return fmt.Errorf("parsing persisted metadata version (%v): %w", versionBytes, err)
		}

		return bucket.Put([]byte(schemaVersionKey), metadataVersionBytes)
	})
}

func getMetadataBucket(tx *bbolt.Tx) (*bbolt.Bucket, error) {
	bucket, err := getBucket(tx, metadataBucketName)
	if err != nil {
		return nil, errors.Wrapf(err, "getting bucket %#q", metadataBucketName)
	}

	return bucket, nil
}

func (ldb *LocalDB) setMetadataValue(name string, value []byte) error {
	return ldb.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := getMetadataBucket(tx)
		if err != nil {
			return err
		}

		return bucket.Put([]byte(name), value)
	})
}

func (ldb *LocalDB) getMetadataValue(name string) ([]byte, error) {
	var value []byte

	err := ldb.db.View(func(tx *bbolt.Tx) error {
		bucket, err := getMetadataBucket(tx)
		if err != nil {
			return err
		}

		value = bucket.Get([]byte(name))

		return nil
	})

	if err != nil {
		return nil, err
	}

	return value, nil
}

func getBucket(tx *bbolt.Tx, name string) (*bbolt.Bucket, error) {
	bucket := tx.Bucket([]byte(name))
	if bucket == nil {
		var err error
		bucket, err = tx.CreateBucket([]byte(name))

		if err != nil {
			return nil, errors.Wrapf(err, "creating bucket %#q", name)
		}
	}

	return bucket, nil
}

func parseUint(buf []byte) (uint64, error) {
	val, err := strconv.ParseUint(string(buf), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %#q as %T: %w", string(buf), val, err)
	}

	return val, nil
}

func formatUint[T constraints.Unsigned](num T) []byte {
	return []byte(strconv.FormatUint(uint64(num), 10))
}
