package dockey

import (
	"fmt"

	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
)

// Pattern represents a collection shard key (the thing you'd pass to
// shardCollection). It's called a "pattern" to distinguish the collection's
// shard key from a document's shard key. The zero value for Pattern
// is useful by itself: it behaves as though a collection has no shard key at
// all.
type Pattern struct {
	raw   bson.Raw
	hasID bool
}

// EmptyPattern is an empty pattern; it can be used to represent unsharded
// collections.
var EmptyPattern Pattern

// NewPattern takes a raw document, like the thing you'd find in
// config.collections. It validates that the shard key is valid BSON, and
// returns a Pattern.
func NewPattern(raw bson.Raw) (Pattern, error) {
	if len(raw) == 0 {
		return Pattern{}, nil
	}

	err := raw.Validate()
	if err != nil {
		return Pattern{}, errors.Wrap(err, "failed to validate shard key pattern")
	}

	_, lookupErr := raw.LookupErr("_id")
	hasID := lookupErr == nil

	return Pattern{raw, hasID}, nil
}

// PatternFromMarshalable takes a marshalable object (like a bson.D) and
// returns a new Pattern. This function panics on any error. It's mostly here
// because it's convenient to use in tests, where you know you'll have a valid
// pattern.
func PatternFromMarshalable(key any) Pattern {
	raw, err := bson.Marshal(key)
	if err != nil {
		panic(err)
	}

	p, err := NewPattern(raw)
	if err != nil {
		panic(err)
	}

	return p
}

// ContainsID returns true if the shard key pattern contains the _id field.
func (p Pattern) ContainsID() bool {
	return p.hasID
}

// IsEmpty returns true if this is a zero-value pattern.
func (p Pattern) IsEmpty() bool {
	return len(p.raw) == 0
}

// Elements returns a slice of bson.RawElement values for every element of the
// shard key. It works like the bson.Raw.Elements method, but does not return
// an error (because we validate the underlying raw document in the
// constructor).
func (p Pattern) Elements() []bson.RawElement {
	elems, err := p.raw.Elements()

	// We've validated this in the constructor, so if this happens then someone
	// has done something nasty.
	if err != nil {
		panic(
			fmt.Sprintf("got error calling raw.Elements() on a validated raw: %s", err),
		)
	}

	return elems
}

// String returns a JSON representation of the pattern.
func (p Pattern) String() string {
	return p.raw.String()
}

// MarshalBSON implements the bson.Marshaler interface.
func (p Pattern) MarshalBSON() ([]byte, error) {
	// We just need to stash the raw here; we can fill back in hasID when we
	// unmarshal.
	return bson.Marshal(p.raw)
}

// MarshalBSON implements the bson.Unmarshaler interface.
func (p *Pattern) UnmarshalBSON(data []byte) error {
	var raw bson.Raw
	err := bson.Unmarshal(data, &raw)
	if err != nil {
		return err
	}

	dupe, err := NewPattern(raw)
	if err != nil {
		return err
	}

	p.raw = dupe.raw
	p.hasID = dupe.hasID
	return nil
}
