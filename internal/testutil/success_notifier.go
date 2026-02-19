package testutil

import (
	"fmt"
	"slices"
	"sync"

	"github.com/10gen/migration-verifier/internal/retry"
)

type MockSuccessNotifier struct {
	messages []string
	mu       sync.Mutex
}

var _ retry.SuccessNotifier = &MockSuccessNotifier{}

func (m *MockSuccessNotifier) NoteSuccess(tmpl string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, fmt.Sprintf(tmpl, args...))
}

func (m *MockSuccessNotifier) Messages() []string {
	return slices.Clone(m.messages)
}
