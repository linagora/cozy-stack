package rag

import (
	"errors"
	"fmt"
	"slices"
)

// retryableError marks a failure that a later run may recover from (network
// error, 5xx from openRAG). A batch hitting one keeps its checkpoint.
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

func retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err: err}
}

func isRetryable(err error) bool {
	var r retryableError
	return errors.As(err, &r)
}

// statusError maps an openRAG response status to nil (2xx, or one of the
// tolerated codes), a retryable error (5xx), or a plain error (other 4xx).
func statusError(what string, status int, tolerated ...int) error {
	if status < 300 || slices.Contains(tolerated, status) {
		return nil
	}
	err := fmt.Errorf("%s status code: %d", what, status)
	if status >= 500 {
		return retryable(err)
	}
	return err
}
