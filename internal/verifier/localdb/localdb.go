// Package localdb implements persisted state in a local datastore.
package localdb

import (
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/dgraph-io/badger/v4"
	"github.com/pkg/errors"
)

type LocalDB struct {
	log *logger.Logger
	db  *badger.DB
}

func New(l *logger.Logger, path string) (*LocalDB, error) {
	db, err := badger.Open(
		badger.DefaultOptions(path).
			WithLogger(&badgerLogger{l}),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "opening %#q", path)
	}

	err = verifySchemaVersion(db)
	if err != nil {
		return nil, errors.Wrap(err, "verifying/setting datastore’s version")
	}

	return &LocalDB{l, db}, nil
}

func (ldb *LocalDB) Close() error {
	return ldb.db.Close()
}
