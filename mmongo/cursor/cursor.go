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
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
)

var (
	clusterTimePath = mslices.Of("$clusterTime", "clusterTime")
)

// Cursor is like mongo.Cursor, but it exposes documents per batch rather than
// a per-document reader. It also exposes cursor metadata, which facilitates
// things like resumable $natural scans.
type Cursor struct {
	sess         mongo.Session
	maxAwaitTime option.Option[time.Duration]
	id           int64
	ns           string
	db           *mongo.Database
	rawResp      bson.Raw
	curBatch     bson.Raw // an undecoded array
	//cursorExtra ExtraMap
}

// ExtraMap represents “extra” data points in cursor metadata.
type ExtraMap = map[string]bson.RawValue

// GetCurrentBatch returns an iterator over the Cursor’s current batch.
func (c *Cursor) GetCurrentBatch() iter.Seq2[bson.Raw, error] {
	batch := c.curBatch

	// NB: This MUST NOT close around c (the receiver), or else there can be
	// a race condition between this callback and GetNext().
	return func(yield func(bson.Raw, error) bool) {
		iterator := &bsoncore.DocumentSequence{
			Style: bsoncore.ArrayStyle,
			Data:  batch,
		}

		for {
			doc, err := iterator.Next()
			if errors.Is(err, io.EOF) {
				return
			}

			if !yield(bson.Raw(doc), err) {
				return
			}
		}
	}
}

// GetClusterTime returns the server response’s cluster time.
func (c *Cursor) GetClusterTime() (primitive.Timestamp, error) {
	ctRV, err := c.rawResp.LookupErr(clusterTimePath...)

	if err != nil {
		return primitive.Timestamp{}, errors.Wrapf(
			err,
			"extracting %#q from server response",
			clusterTimePath,
		)
	}

	ts, err := mbson.CastRawValue[primitive.Timestamp](ctRV)
	if err != nil {
		return primitive.Timestamp{}, errors.Wrapf(
			err,
			"parsing server response’s %#q",
			clusterTimePath,
		)
	}

	return ts, nil
}

// IsFinished indicates whether the present batch is the final one.
func (c *Cursor) IsFinished() bool {
	return c.id == 0
}

// GetNext fetches the next batch of responses from the server and caches it
// for access via GetCurrentBatch().
//
// extraPieces are things you want to add to the underlying `getMore`
// server call, such as `batchSize`.
func (c *Cursor) GetNext(ctx context.Context, extraPieces ...bson.E) error {
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

	//c.curBatch = extractBatch(raw, "nextBatch") baseResp.Cursor.NextBatch
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
) (*Cursor, error) {
	raw, err := resp.Raw()
	if err != nil {
		return nil, errors.Wrapf(err, "cursor open failed")
	}

	baseResp := baseResponse{}

	err = bson.Unmarshal(raw, &baseResp)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to decode cursor-open response to %T", baseResp)
	}

	return &Cursor{
		db:       db,
		id:       baseResp.Cursor.ID,
		ns:       baseResp.Cursor.Ns,
		rawResp:  raw,
		curBatch: baseResp.Cursor.FirstBatch,
	}, nil
}

func (c *Cursor) SetSession(sess mongo.Session) {
	c.sess = sess
}

func (c *Cursor) SetMaxAwaitTime(d time.Duration) {
	c.maxAwaitTime = option.Some(d)
}

// GetResumeToken is a convenience function that extracts the
// post-batch resume token from the cursor.
func GetResumeToken(c *Cursor) (bson.Raw, error) {
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
