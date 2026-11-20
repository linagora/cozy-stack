package files

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type lookupErrorVFS struct {
	vfs.VFS
	err error
}

func (f lookupErrorVFS) DirOrFileByPath(string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	return nil, nil, f.err
}

func TestDriveTargetByPathErrors(t *testing.T) {
	scope := sharing.FilePathScope{Roots: []vfs.LinkRoot{{Path: "/Owner/Drive"}}}
	const p = "/Drive/Secret plan.txt"
	status := func(err error) (int, string) {
		var errj *jsonapi.Error
		require.ErrorAs(t, err, &errj)
		return errj.Status, errj.Detail
	}

	_, _, err := lookupPath(lookupErrorVFS{err: os.ErrNotExist}, scope, p)
	missing, missingDetail := status(err)
	assert.Equal(t, http.StatusNotFound, missing)

	_, _, err = lookupPath(lookupErrorVFS{err: vfs.ErrNonAbsolutePath}, scope, p)
	code, detail := status(err)
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, missingDetail, detail)

	_, _, err = lookupPath(lookupErrorVFS{err: errors.New("couchdb: connection refused " + p)}, scope, p)
	code, detail = status(err)
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.NotContains(t, detail, "Secret")
	assert.NotContains(t, detail, "couchdb")
}
