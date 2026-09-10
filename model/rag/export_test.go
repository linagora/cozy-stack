package rag

import (
	"github.com/cozy/cozy-stack/model/instance"
)

// RootWorkspaceID is the openRAG workspace id of the root folder.
const RootWorkspaceID = rootWorkspaceID

// ReconcileWorkspacesForTest runs the workspace reconciliation against the
// assistants of the instance, with own the folder of the running reconcile
// job (only used when push is nil) and push the reconcile job hook.
func ReconcileWorkspacesForTest(inst *instance.Instance, own string, push func(dirID string) error) error {
	sc, err := loadScopes(inst, TestingLogger())
	if err != nil {
		return err
	}
	return reconcileWorkspaces(inst, TestingLogger(), inst.RAGServer(), sc, own, push)
}

// SetReconcilePushForTest replaces the reconcile job push; it returns a
// function restoring the real one (call it in t.Cleanup).
func SetReconcilePushForTest(fn func(inst *instance.Instance, dirID string) error) func() {
	old := pushReconcile
	pushReconcile = fn
	return func() { pushReconcile = old }
}

// IsRetryableForTest tells whether the error asks for another attempt (a
// transient failure), as opposed to one that is logged and skipped.
func IsRetryableForTest(err error) bool {
	return isRetryable(err)
}
