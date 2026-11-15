package vfs

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
