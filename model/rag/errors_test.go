package rag

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryable(t *testing.T) {
	assert.Nil(t, retryable(nil))
	base := errors.New("boom")
	err := retryable(base)
	assert.True(t, isRetryable(err))
	assert.True(t, errors.Is(err, base))
	assert.False(t, isRetryable(base))
	assert.True(t, isRetryable(errors.Join(errors.New("other"), err)))
}

func TestStatusError(t *testing.T) {
	assert.NoError(t, statusError("x", 200))
	assert.NoError(t, statusError("x", 201))
	assert.NoError(t, statusError("x", 404, 404))
	assert.NoError(t, statusError("x", 409, 404, 409))

	err := statusError("x", 404)
	require.Error(t, err)
	assert.False(t, isRetryable(err))
	assert.Contains(t, err.Error(), "x status code: 404")

	err = statusError("x", 503)
	require.Error(t, err)
	assert.True(t, isRetryable(err))
}

func TestIndexMessageNames(t *testing.T) {
	global := IndexMessage{Doctype: "io.cozy.files"}
	assert.Equal(t, "index/io.cozy.files", global.lockName())
	assert.Equal(t, "rag-index", global.checkpointID())

	scoped := IndexMessage{Doctype: "io.cozy.files", DirID: "abc"}
	assert.Equal(t, "index/io.cozy.files/abc", scoped.lockName())
	assert.Equal(t, "rag-index-abc", scoped.checkpointID())
}
