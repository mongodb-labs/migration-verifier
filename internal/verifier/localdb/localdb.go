// Package localdb implements persisted state in a local datastore.
package localdb

import (
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/dgraph-io/badger/v4"
	"github.com/pkg/errors"
)

// LocalDB provides local storage for performance-sensitive parts of
// the verifier’s internal state.
type LocalDB struct {
	log *logger.Logger
	db  *badger.DB
}

// New returns a new LocalDB instance. If the datastore already exists it
// will verify that the schema version matches.
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
