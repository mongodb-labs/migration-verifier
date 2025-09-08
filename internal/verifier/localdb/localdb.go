// Package localdb implements persisted state in a local datastore.
package localdb

import (
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/pkg/errors"
	"go.etcd.io/bbolt"
)

type LocalDB struct {
	log *logger.Logger
	db  *bbolt.DB
}

func New(l *logger.Logger, path string) (*LocalDB, error) {
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "opening %#q", path)
	}

	err = verifySchemaVersion(db)
	if err != nil {
		return nil, errors.Wrap(err, "verifying/setting datastore’s version")
	}

	return &LocalDB{l, db}, nil
}
