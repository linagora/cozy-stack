package sharings_test

import (
	"net/http"
	"testing"

	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createNoteForPendingMember(t *testing.T, e *httpexpect.Expect, path, dirID, title, token string) string {
	t.Helper()
	return e.POST(path).
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(`{
			"data": {
				"type": "io.cozy.notes.documents",
				"attributes": {
					"title": "` + title + `",
					"dir_id": "` + dirID + `",
					"schema": {
						"nodes": [
							["doc", {"content": "block+"}],
							["paragraph", {"content": "inline*", "group": "block"}],
							["text", {"group": "inline"}]
						],
						"marks": [],
						"topNode": "doc"
					}
				}
			}
		}`)).
		Expect().Status(http.StatusCreated).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.id").String().NotEmpty().Raw()
}

func permissionTypeForPendingMember(t *testing.T, e *httpexpect.Expect, code string) string {
	t.Helper()
	return e.GET("/permissions/self").
		WithHeader("Authorization", "Bearer "+code).
		Expect().Status(http.StatusOK).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.attributes.type").String().Raw()
}

// A short preview code does not tell which member uses it: the request is
// refused, whatever the other members of the drive are.
func TestOpenDriveFileWithShortPreviewCodeAndPendingMember(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, _, eD := env.createClients(t)
	eOpen := newOpenClient(t, env.acme)

	// Dave is a read-only member, and Charlie has not accepted yet.
	driveID, driveRootID, _ := createSharedDrive(t, DriveCreationMethodFromFolder,
		env.acme, env.acmeToken, env.tsA.URL, "Pending member drive", "drive",
		[]RecipientInfo{
			{Name: "Dave", Email: "dave@example.net", ReadOnly: true},
			{Name: "Charlie", Email: "charlie@example.net"},
		})
	files := []struct{ route, id string }{
		{"office", createFileWithMime(t, eA, driveRootID, "pending.pdf", env.acmeToken, "application/pdf")},
		{"notes", createNoteForPendingMember(t, eOpen, "/notes", driveRootID, "pending note", env.acmeToken)},
		{"editor", createFile(t, eA, driveRootID, "pending.txt", env.acmeToken)},
	}
	noteID := createNoteForPendingMember(t, eA, "/sharings/drives/"+driveID+"/notes", driveRootID, "Pending note", env.acmeToken)
	acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, driveID)

	previewCode := eD.GET("/sharings/drives/"+driveID+"/notes/"+noteID+"/open").
		WithHeader("Authorization", "Bearer "+env.daveToken).
		Expect().Status(http.StatusOK).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.attributes.sharecode").String().NotEmpty().Raw()
	require.Equal(t, "share-preview", permissionTypeForPendingMember(t, eA, previewCode))

	for _, f := range files {
		t.Run(f.route, func(t *testing.T) {
			res := eOpen.GET("/"+f.route+"/"+f.id+"/open").
				WithHeader("Authorization", "Bearer "+previewCode).
				Expect()
			status := res.Raw().StatusCode
			assert.Equal(t, http.StatusNotFound, status)
			if status == http.StatusOK {
				code := res.JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
					Object().Path("$.data.attributes.sharecode").String().Raw()
				t.Errorf("a code has been given with the type %s", permissionTypeForPendingMember(t, eA, code))
			}
		})
	}
}
