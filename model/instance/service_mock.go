package instance

import (
	"testing"

	"github.com/stretchr/testify/mock"
)

// Mock implementation of
type Mock struct {
	mock.Mock
}

// NewMock instantiates a new [Mock].
func NewMock(t *testing.T) *Mock {
	m := new(Mock)
	m.Test(t)

	t.Cleanup(func() { m.AssertExpectations(t) })

	return m
}

// Get mock method.
func (m *Mock) Get(domain string) (*Instance, error) {
	args := m.Called(domain)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*Instance), args.Error(1)
}

// Update mock method.
func (m *Mock) Update(inst *Instance) error {
	return m.Called(inst).Error(0)
}

// Delete mock method.
func (m *Mock) Delete(inst *Instance) error {
	return m.Called(inst).Error(0)
}

// CheckPassphrase mock method.
func (m *Mock) CheckPassphrase(inst *Instance, pass []byte) error {
	return m.Called(inst, pass).Error(0)
}

// SetEmail mock method.
func (m *Mock) SetEmail(inst *Instance, email string) error {
	return m.Called(inst, email).Error(0)
}
