package localdb

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"

	"github.com/dgraph-io/badger/v4"
	"golang.org/x/exp/constraints"
)

const (
	// If we ever change how the data is stored in this DB,
	// bump this version to invalidate any verifications using
	// the prior schema.
	schemaVersion = uint16(1)

	schemaVersionKey = "formatVersion"

	metadataBucketName = "metadata"
)

func verifySchemaVersion(db *badger.DB) error {
	metadataVersionBytes := formatUint(schemaVersion)

	return db.Update(func(tx *badger.Txn) error {
		item, err := tx.Get([]byte(metadataBucketName + "." + schemaVersionKey))

		if err != nil {
			if !errors.Is(err, badger.ErrKeyNotFound) {
				return err
			}
		} else {
			versionBytes, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			if bytes.Equal(versionBytes, metadataVersionBytes) {
				return nil
			}
			foundVersion, err := parseUint(versionBytes)
			if err != nil {
				return fmt.Errorf("found metadata version %d, but %d is required; is this local DB from a prior verifier version?", foundVersion, schemaVersion)
			}

			return fmt.Errorf("parsing persisted metadata version (%v): %w", versionBytes, err)
		}

		return tx.Set([]byte(metadataBucketName+"."+schemaVersionKey), metadataVersionBytes)
	})
}

func (ldb *LocalDB) setMetadataValue(name string, value []byte) error {
	return ldb.db.Update(func(tx *badger.Txn) error {
		return tx.Set([]byte(metadataBucketName+"."+name), value)
	})
}

func (ldb *LocalDB) getMetadataValue(name string) ([]byte, error) {
	var value []byte

	err := ldb.db.View(func(tx *badger.Txn) error {
		item, err := tx.Get([]byte(metadataBucketName + "." + name))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}

		value, err = item.ValueCopy(value)
		return err
	})

	if err != nil {
		return nil, err
	}

	return value, nil
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
