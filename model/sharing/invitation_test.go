package sharing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanFilename(t *testing.T) {
	cases := map[string]string{
		"foo":             "foo",
		"invalid <chars>": "invalid -chars-",
	}
	for filename, expected := range cases {
		t.Run(filename, func(t *testing.T) {
			assert.Equal(t, expected, cleanFilename(filename))
		})
	}
}

func TestDelegatedInvitationState(t *testing.T) {
	tests := []struct {
		name   string
		member Member
		states map[string]string
		state  string
		ok     bool
	}{
		{
			name:   "uses instance when member has instance and email",
			member: Member{Email: "alice@example.test", Instance: "https://alice.example.test"},
			states: map[string]string{
				"alice@example.test":         "email-state",
				"https://alice.example.test": "instance-state",
			},
			state: "instance-state",
			ok:    true,
		},
		{
			name:   "falls back to email",
			member: Member{Email: "alice@example.test"},
			states: map[string]string{"alice@example.test": "email-state"},
			state:  "email-state",
			ok:     true,
		},
		{
			name:   "rejects missing state",
			member: Member{Email: "alice@example.test"},
			states: map[string]string{},
		},
		{
			name:   "rejects empty state",
			member: Member{Email: "alice@example.test"},
			states: map[string]string{"alice@example.test": ""},
		},
		{
			name:   "rejects member without address",
			member: Member{Name: "Alice"},
			states: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, ok := delegatedInvitationState(tt.member, tt.states)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.state, state)
		})
	}
}
