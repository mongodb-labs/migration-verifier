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

// UnmarshalCursor is like mongo.Cursor.All, excet that it uses BSONUnmarshaler
// rather than the driver’s own unmarshaling. This avoids reflection & so is
// much faster.
//
// Because Go generics can’t distinguish value vs. pointer receivers, this
// can’t check that the type argument is a BSONUnmarshaler until runtime.
func UnmarshalCursor[V any](
	ctx context.Context,
	cursor *mongo.Cursor,
	in []V,
) ([]V, error) {
	var raws []bson.Raw
	if err := cursor.All(ctx, &raws); err != nil {
		return nil, err
	}

	for _, raw := range raws {
		p := new(V)

		// Runtime check: does *V implement BSONUnmarshaler?
		um, ok := any(p).(BSONUnmarshaler)
		if !ok {
			panic(fmt.Sprintf("*%T does not implement BSONUnmarshaler", p))
		}

		if err := um.UnmarshalFromBSON([]byte(raw)); err != nil {
			return nil, err
		}

		in = append(in, *p)
	}

	return in, nil
}
