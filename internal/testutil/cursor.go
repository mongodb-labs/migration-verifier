package testutil

import (
	"context"

	"github.com/10gen/migration-verifier/mmongo/cursor"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// MockAbstractCursor lets you mock cursors in tests.
type MockAbstractCursor struct {
	Batches [][]bson.Raw
	Error   error

	cur bson.Raw
}

var _ cursor.Abstract = &MockAbstractCursor{}

func (m *MockAbstractCursor) Next(context.Context) bool {
	m.cur = nil

	if len(m.Batches) == 0 {
		return false
	}

	lo.Assertf(
		len(m.Batches[0]) > 0,
		"initial batch must be nonempty",
	)

	m.cur = m.Batches[0][0]
	m.Batches[0] = m.Batches[0][1:]

	for len(m.Batches[0]) == 0 {
		m.Batches = m.Batches[1:]
	}

	return true
}

func (m *MockAbstractCursor) TryNext(ctx context.Context) bool {
	return m.Next(ctx)
}

func (m *MockAbstractCursor) Current() bson.Raw {
	return m.cur
}

func (m *MockAbstractCursor) Err() error {
	return m.Error
}

func (m *MockAbstractCursor) RemainingBatchLength() int {
	if len(m.Batches) == 0 {
		return 0
	}

	return len(m.Batches[0])
}
