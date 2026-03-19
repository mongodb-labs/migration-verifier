package testutil

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/10gen/migration-verifier/internal/retry"
)

type MockSuccessNotifier struct {
	T        *testing.T
	messages []string
	mu       sync.Mutex
}

var _ retry.SuccessNotifier = &MockSuccessNotifier{}

func (m *MockSuccessNotifier) NoteSuccess(tmpl string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.T != nil {
		m.T.Logf(tmpl, args...)
	}

	m.messages = append(m.messages, fmt.Sprintf(tmpl, args...))
}

func (m *MockSuccessNotifier) Messages() []string {
	return slices.Clone(m.messages)
}
