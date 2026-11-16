package vfs

import (
	"sync/atomic"
	"time"
)

// StopUploadAfter makes the end of the uploads stop after the given step with
// err, as a stop of the stack would. It returns a function to restore the
// normal behavior.
func StopUploadAfter(step string, err error) func() {
	uploadStepHook = func(s string) error {
		if s == step {
			return err
		}
		return nil
	}
	return func() { uploadStepHook = nil }
}

// SetClaimWait changes the time a new request waits for an older one. It
// returns a function to restore the normal value.
func SetClaimWait(d time.Duration) func() {
	previous := claimWait
	claimWait = d
	return func() { claimWait = previous }
}

// CountClaimReads counts the reads of the uploads made by the requests that
// wait for an older one. It returns the counter and a function to restore the
// normal behavior.
func CountClaimReads() (*atomic.Int64, func()) {
	var reads atomic.Int64
	previous := pollUpload
	pollUpload = func(fs VFS, id string) (*Upload, error) {
		reads.Add(1)
		return previous(fs, id)
	}
	return &reads, func() { pollUpload = previous }
}

// ActiveUploads returns the uploads that are not completed.
var ActiveUploads = activeUploads

// SetUploadStepHook calls hook at each step of the uploads. It returns a
// function to restore the normal behavior.
func SetUploadStepHook(hook func(step string) error) func() {
	uploadStepHook = hook
	return func() { uploadStepHook = nil }
}
