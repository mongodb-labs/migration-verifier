package cursor

import (
	"context"

	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// CursorLike defines the methods that concrete cursor types must implement.
type CursorLike interface {
	Next(context.Context) bool
	TryNext(context.Context) bool
	RemainingBatchLength() int
	Err() error
}

type cursorType interface {
	*mongo.Cursor | *mongo.ChangeStream
}

// Abstract lets you mock cursor iteration easily in tests. By passing this
// interface around rather than a concrete type in your production code,
// you can test that code easily with a mock implementation.
type Abstract interface {
	CursorLike
	Current() bson.Raw
}

type abstractImpl struct {
	CursorLike
}

// NewAbstract returns a new Abstract from one of the recognized cursor types.
func NewAbstract[T cursorType](c T) Abstract {
	return &abstractImpl{any(c).(CursorLike)}
}

func (a *abstractImpl) Current() bson.Raw {
	switch c := a.CursorLike.(type) {
	case *mongo.Cursor:
		return c.Current
	case *mongo.ChangeStream:
		return c.Current
	}

	lo.Assertf(false, "unknown cursor type: %T", a.CursorLike)

	// Go doesn’t recognize Assertf as a panic.
	return nil
}
