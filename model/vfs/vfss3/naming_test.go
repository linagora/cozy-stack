package vfss3

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeBucketName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"production", "production"},
		{"my_context", "my-context"},
		{"My.Context", "my-context"},
		{"UPPERCASE", "uppercase"},
		{"with spaces!", "withspaces"},
		{"a--b--c", "a-b-c"},
		{"-leading-trailing-", "leading-trailing"},
		{"very-long-name-that-exceeds-the-maximum-allowed-length", "very-long-name-that-exceeds-the-maxim"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, sanitizeBucketName(tt.input))
		})
	}
}

func TestBucketName(t *testing.T) {
	tests := []struct {
		orgID        string
		bucketPrefix string
		expected     string
	}{
		{"org-123", "cozy", "cozy-org-123"},
		{"", "cozy", "cozy-default"},
		{"My_Org", "cozy", "cozy-my-org"},
		{"org.example.com", "cozy", "cozy-org-example-com"},
	}

	for _, tt := range tests {
		t.Run(tt.orgID, func(t *testing.T) {
			assert.Equal(t, tt.expected, BucketName(tt.orgID, tt.bucketPrefix))
		})
	}
}

func TestMakeObjectKey(t *testing.T) {
	// Standard 32-char docID and 16-char internalID
	key := MakeObjectKey("alice.example.com/", "abcdefghijklmnopqrstuvwxyz012345", "0123456789abcdef")
	assert.Equal(t, "alice.example.com/abcdefghijklmnopqrstuv/wxyz0/12345/0123456789abcdef", key)

	// Non-standard lengths
	key = MakeObjectKey("alice.example.com/", "short", "id")
	assert.Equal(t, "alice.example.com/short/id", key)
}

func TestMakeDocID(t *testing.T) {
	// Standard 51-char object name
	docID, internalID := makeDocID("abcdefghijklmnopqrstuv/wxyz0/12345/0123456789abcdef")
	assert.Equal(t, "abcdefghijklmnopqrstuvwxyz012345", docID)
	assert.Equal(t, "0123456789abcdef", internalID)

	// Non-standard
	docID, internalID = makeDocID("short/id")
	assert.Equal(t, "short", docID)
	assert.Equal(t, "id", internalID)
}
