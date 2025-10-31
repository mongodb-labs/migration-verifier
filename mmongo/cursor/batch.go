// Package cursor exposes a cursor implementation that facilitates easy
// batch reads as well as reading of custom cursor properties like
// resume tokens.
package cursor

import (
	"context"
	"fmt"
	"io"
	"iter"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

var (
	clusterTimePath = mslices.Of("$clusterTime", "clusterTime")
)

// BatchCursor is like mongo.Cursor, but it exposes documents per batch rather than
// a per-document reader. This facilitates more efficient memory usage
// because there is no need to clone each document individually, as
// mongo.Cursor requires.
type BatchCursor struct {
	sess         *mongo.Session
	maxAwaitTime option.Option[time.Duration]
	id           int64
	ns           string
	db           *mongo.Database
	rawResp      bson.Raw
	curBatch     bson.RawArray
}

// GetCurrentBatchIterator returns an iterator over the BatchCursor’s current batch.
// Note that the iteratees are NOT copied; the expectation is that each batch
// will be iterated exactly once. (Nothing *requires* that, of course.)
func (c *BatchCursor) GetCurrentBatchIterator() iter.Seq2[bson.Raw, error] {
	// NB: Use of iter.Seq2 to return an error is a bit controversial.
	// The pattern is used here in order to minimize the odds that a caller
	// would overlook the need to check the error, which seems more probable
	// with various other patterns.
	//
	// See “https://sinclairtarget.com/blog/2025/07/error-handling-with-iterators-in-go/”.

	batch := c.curBatch

	// NB: This MUST NOT close around c (the receiver), or else there can be
	// a race condition between this callback and GetNext().
	return func(yield func(bson.Raw, error) bool) {
		iterator := &bsoncore.Iterator{
			List: bsoncore.Array(batch),
		}

		for {
			val, err := iterator.Next()
			if errors.Is(err, io.EOF) {
				return
			}

			doc, ok := val.DocumentOK()
			if !ok {
				err = fmt.Errorf("expected BSON %s but found %s", bson.TypeEmbeddedDocument, val.Type)
			}

			if !yield(bson.Raw(doc), err) {
				return
			}

			if err != nil {
				panic(fmt.Sprintf("Iteration must stop after error (%v)", err))
			}
		}
	}
}

// GetClusterTime returns the server response’s cluster time.
func (c *BatchCursor) GetClusterTime() (bson.Timestamp, error) {
	ctRV, err := c.rawResp.LookupErr(clusterTimePath...)

	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(
			err,
			"extracting %#q from server response",
			clusterTimePath,
		)
	}

	ts, err := mbson.CastRawValue[bson.Timestamp](ctRV)
	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(
			err,
			"parsing server response’s %#q",
			clusterTimePath,
		)
	}

	return ts, nil
}

// IsFinished indicates whether the present batch is the final one.
func (c *BatchCursor) IsFinished() bool {
	return c.id == 0
}

// GetNext fetches the next batch of responses from the server and caches it
// for access via GetCurrentBatch().
//
// extraPieces are things you want to add to the underlying `getMore`
// server call, such as `batchSize`.
func (c *BatchCursor) GetNext(ctx context.Context, extraPieces ...bson.E) error {
	if c.IsFinished() {
		panic("internal error: cursor already finished!")
	}

	nsDB, nsColl, found := strings.Cut(c.ns, ".")
	if !found {
		panic("Malformed namespace from cursor (expect a dot): " + c.ns)
	}
	if nsDB != c.db.Name() {
		panic(fmt.Sprintf("db from cursor (%s) mismatches db struct (%s)", nsDB, c.db.Name()))
	}

	cmd := bson.D{
		{"getMore", c.id},
		{"collection", nsColl},
	}

	if awaitTime, has := c.maxAwaitTime.Get(); has {
		cmd = append(cmd, bson.E{"maxTimeMS", awaitTime.Milliseconds()})
	}

	cmd = append(cmd, extraPieces...)

	if c.sess != nil {
		ctx = mongo.NewSessionContext(ctx, c.sess)
	}
	resp := c.db.RunCommand(ctx, cmd)

	raw, err := resp.Raw()
	if err != nil {
		return fmt.Errorf("iterating %#q’s cursor: %w", c.ns, err)
	}

	c.curBatch = lo.Must(raw.LookupErr("cursor", "nextBatch")).Array()
	c.rawResp = raw
	//c.cursorExtra = baseResp.Cursor.Extra
	c.id = lo.Must(raw.LookupErr("cursor", "id")).AsInt64() //baseResp.Cursor.ID

	return nil
}

type cursorResponse struct {
	ID int64
	Ns string

	// These are both BSON arrays. We use bson.Raw here to delay parsing
	// and avoid allocating a large slice.
	FirstBatch bson.Raw
	NextBatch  bson.Raw
}

type baseResponse struct {
	Cursor cursorResponse
}

// New creates a Cursor from the response of a cursor-returning command
// like `find` or `bulkWrite`.
//
// Use this control (rather than the Go driver’s cursor implementation)
// to extract parts of the cursor responses that the driver’s API doesn’t
// expose. This is useful, e.g., to do a resumable $natural scan by
// extracting resume tokens from `find` responses.
//
// See NewWithSession() as well.
func New(
	db *mongo.Database,
	resp *mongo.SingleResult,
) (*BatchCursor, error) {
	raw, err := resp.Raw()
	if err != nil {
		return nil, errors.Wrapf(err, "cursor open failed")
	}

	baseResp := baseResponse{}

	err = bson.Unmarshal(raw, &baseResp)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to decode cursor-open response to %T", baseResp)
	}

	return &BatchCursor{
		db:       db,
		id:       baseResp.Cursor.ID,
		ns:       baseResp.Cursor.Ns,
		rawResp:  raw,
		curBatch: bson.RawArray(baseResp.Cursor.FirstBatch),
	}, nil
}

func (c *BatchCursor) SetSession(sess *mongo.Session) {
	c.sess = sess
}

func (c *BatchCursor) SetMaxAwaitTime(d time.Duration) {
	c.maxAwaitTime = option.Some(d)
}

// GetResumeToken is a convenience function that extracts the
// post-batch resume token from the cursor.
func GetResumeToken(c *BatchCursor) (bson.Raw, error) {
	var resumeToken bson.Raw

	tokenRV, err := c.rawResp.LookupErr("cursor", "postBatchResumeToken")
	if err != nil {
		return nil, errors.Wrapf(err, "extracting change stream’s resume token")
	}

	resumeToken, err = mbson.CastRawValue[bson.Raw](tokenRV)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"parsing change stream’s resume token",
		)
	}

	return resumeToken, nil
}
