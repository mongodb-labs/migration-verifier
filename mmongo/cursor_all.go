package mmongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type BSONUnmarshaler interface {
	UnmarshalFromBSON([]byte) error
}

func UnmarshalCursor[V any](
	ctx context.Context,
	cursor *mongo.Cursor,
) ([]V, error) {
	var raws []bson.Raw
	if err := cursor.All(ctx, &raws); err != nil {
		return nil, err
	}

	ret := make([]V, len(raws))
	for i, raw := range raws {
		// Take a pointer to the slice element (type *V).
		p := &ret[i]

		// Runtime check: does *V implement BSONUnmarshaler?
		um, ok := any(p).(BSONUnmarshaler)
		if !ok {
			panic(fmt.Sprintf("*%T does not implement BSONUnmarshaler", p))
		}

		if err := um.UnmarshalFromBSON([]byte(raw)); err != nil {
			return nil, err
		}
	}

	return ret, nil
}

/*
func UnmarshalCursor[T BSONUnmarshaler](
	ctx context.Context,
	cursor *mongo.Cursor,
) ([]T, error) {

	return unmarshalCursorInternal[T, *T](ctx, cursor)
}

func unmarshalCursorInternal[V any, P bsonUnmarshalerPointer[V]](
	ctx context.Context,
	cursor *mongo.Cursor,
) ([]V, error) {
	var raws []bson.Raw

	if err := cursor.All(ctx, &raws); err != nil {
		return nil, err
	}

	ret := make([]V, len(raws))
	for i, raw := range raws {
		if err := P(&ret[i]).UnmarshalFromBSON(raw); err != nil {
			return nil, err
		}
	}

	return ret, nil
}
*/
