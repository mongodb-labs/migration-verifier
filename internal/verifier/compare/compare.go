package compare

import (
	pool "github.com/libp2p/go-buffer-pool"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// DocWithTS holds a document that’s to be compared with its peer’s or, if not,
// marked as missing/extra on the destination.
type DocWithTS struct {
	Doc    bson.Raw
	TS     bson.Timestamp
	manual bool
}

// NewDocWithTS copies the given document to a manually-managed memory pool
// then returns a struct containing that copy.
//
// IMPORTANT: You *MUST* call Release() on the returned struct, or else
// you’ll leak memory.
func NewDocWithTS(doc bson.Raw, ts bson.Timestamp) DocWithTS {
	copiedDoc := pool.Get(len(doc))
	copy(copiedDoc, doc)

	return DocWithTS{
		Doc:    copiedDoc,
		TS:     ts,
		manual: true,
	}
}

// Release releases the memory allocated to hold the document.
// If the struct was not created with NewDocWithTS, this panics.
func (d DocWithTS) Release() {
	lo.Assertf(
		d.manual,
		"Release() called on auto-managed %T",
		d,
	)

	pool.Put(d.Doc)
}
