package sharings_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createNoteInDriveForDriveToken(t *testing.T, e *httpexpect.Expect, driveID, dirID, title, token string) string {
	t.Helper()
	return e.POST("/sharings/drives/"+driveID+"/notes").
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

func sharecodeForDriveToken(t *testing.T, e *httpexpect.Expect, path, token string) string {
	t.Helper()
	return e.GET(path).
		WithHeader("Authorization", "Bearer "+token).
		Expect().Status(http.StatusOK).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.attributes.sharecode").String().NotEmpty().Raw()
}

func permissionTypeForDriveToken(t *testing.T, e *httpexpect.Expect, code string) string {
	t.Helper()
	return e.GET("/permissions/self").
		WithHeader("Authorization", "Bearer "+code).
		Expect().Status(http.StatusOK).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.attributes.type").String().Raw()
}

// createCozyToCozySharingInDrive creates a regular sharing on the folder through
// the API, and activates it like Betty accepting it would do. It does not depend
// on a real acceptance: the only needed effects are the active flag and the
// io.cozy.shared documents, that Sharing.Setup creates with InitialIndex.
func createCozyToCozySharingInDrive(t *testing.T, e *httpexpect.Expect, inst *instance.Instance, dirID, token string) *sharing.Sharing {
	t.Helper()
	contact := createContact(t, inst, "Betty", "betty@example.net")
	sharingID := e.POST("/sharings/").
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", jsonAPIContentType).
		WithBytes(mustJSON(t, map[string]interface{}{
			"data": map[string]interface{}{
				"type": consts.Sharings,
				"attributes": map[string]interface{}{
					"description": "Exchanged with Betty",
					"rules": []map[string]interface{}{{
						"title":   "Exchanged",
						"doctype": consts.Files,
						"values":  []string{dirID},
						"add":     "sync",
						"update":  "sync",
						"remove":  "sync",
					}},
				},
				"relationships": map[string]interface{}{
					"recipients": map[string]interface{}{
						"data": []map[string]interface{}{{"id": contact.ID(), "type": consts.Contacts}},
					},
				},
			},
		})).
		Expect().Status(http.StatusCreated).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.id").String().NotEmpty().Raw()

	s, err := sharing.FindSharing(inst, sharingID)
	require.NoError(t, err)
	s.Active = true
	s.Members[1].Status = sharing.MemberStatusReady
	require.NoError(t, couchdb.UpdateDoc(inst, s))
	require.NoError(t, couchdb.EnsureDBExist(inst, consts.Shared))
	require.NoError(t, s.InitialIndex(inst, s.Rules[0], 0))
	return s
}

// statusOfPut returns the status of a write of the file content with the code.
func statusOfPut(e *httpexpect.Expect, fileID, code string) int {
	return e.PUT("/files/"+fileID).
		WithHeader("Authorization", "Bearer "+code).
		WithHeader("Content-Type", "text/plain").
		WithBytes([]byte("must not be writable")).
		Expect().Raw().StatusCode
}

// A read-only member of a drive gets a read-only code for a file of this drive,
// even when the file is also in a cozy-to-cozy sharing of the owner.
func TestOpenFileOfDriveAlsoInCozyToCozySharingForReadOnlyMember(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, _, eD := env.createClients(t)

	// Dave is a read-only member of the drive.
	driveID, driveRootID, _ := createSharedDrive(t, DriveCreationMethodFromFolder,
		env.acme, env.acmeToken, env.tsA.URL, "Open c2c drive", "drive",
		[]RecipientInfo{{Name: "Dave", Email: "dave@example.net", ReadOnly: true}})
	exchangedDirID := createDirectory(t, eA, driveRootID, "Exchanged", env.acmeToken)
	noteID := createNoteInDriveForDriveToken(t, eA, driveID, exchangedDirID, "Exchanged note", env.acmeToken)
	editorFileID := createFileWithMime(t, eA, exchangedDirID, "drawing.excalidraw", env.acmeToken, "application/json")
	noteFileID := createFile(t, eA, driveRootID, "read-only-note.txt", env.acmeToken)
	editorTargetID := createFile(t, eA, driveRootID, "read-only-editor.txt", env.acmeToken)
	acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, driveID)

	// Control: before the cozy-to-cozy sharing, Dave gets a read-only code.
	code := sharecodeForDriveToken(t, eD, "/sharings/drives/"+driveID+"/notes/"+noteID+"/open", env.daveToken)
	require.Equal(t, "share-preview", permissionTypeForDriveToken(t, eA, code))

	// The owner shares a folder of the drive with Betty. Dave is not a member
	// of this sharing.
	createCozyToCozySharingInDrive(t, eA, env.acme, exchangedDirID, env.acmeToken)

	cases := []struct {
		name, path, targetID string
	}{
		{"Note", "/sharings/drives/" + driveID + "/notes/" + noteID + "/open", noteFileID},
		{"Editor", "/sharings/drives/" + driveID + "/editor/" + editorFileID + "/open", editorTargetID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := sharecodeForDriveToken(t, eD, tc.path, env.daveToken)
			assert.Equal(t, "share-preview", permissionTypeForDriveToken(t, eA, code),
				"a read-only member must get a read-only code")
			assert.Equal(t, http.StatusForbidden, statusOfPut(eA, tc.targetID, code),
				"the code given to a read-only member must not write a file of the drive")
		})
	}
}

// driveTokenPeer stands for the instance of the other member of a cozy-to-cozy
// sharing, where a file created on this instance is opened.
type driveTokenPeer struct {
	*httptest.Server
	mu    sync.Mutex
	calls []*http.Request
}

func newDriveTokenPeer(t *testing.T) *driveTokenPeer {
	t.Helper()
	peer := &driveTokenPeer{}
	peer.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.mu.Lock()
		peer.calls = append(peer.calls, r)
		peer.mu.Unlock()
		w.Header().Set("Content-Type", jsonAPIContentType)
		_, _ = w.Write([]byte(`{"data": {"type": "peer", "id": "peer", "attributes": {"sharecode": "code-from-peer"}}}`))
	}))
	t.Cleanup(peer.Close)
	return peer
}

// takeCalls returns the requests received since the last call.
func (p *driveTokenPeer) takeCalls() []*http.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	calls := p.calls
	p.calls = nil
	return calls
}

// markFileCreatedOn makes the file look like it has been created on another
// instance of the sharing, that is the instance where it should be opened.
func markFileCreatedOn(t *testing.T, inst *instance.Instance, fileID, instanceURL string) {
	t.Helper()
	file, err := inst.VFS().FileByID(fileID)
	require.NoError(t, err)
	require.NotNil(t, file.CozyMetadata)
	file.CozyMetadata.CreatedOn = instanceURL
	require.NoError(t, couchdb.UpdateDoc(inst, file))
}

// The code of a drive opens a file through this drive, and the code of a
// cozy-to-cozy sharing through this sharing, when the file is in both of them.
func TestOpenFileOfDriveAlsoInCozyToCozySharingKeepsEachCode(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, eD := env.createClients(t)

	driveID, driveRootID, _ := createSharedDrive(t, DriveCreationMethodFromFolder,
		env.acme, env.acmeToken, env.tsA.URL, "Each code drive", "drive",
		[]RecipientInfo{
			{Name: "Betty", Email: "betty@example.net"},
			{Name: "Dave", Email: "dave@example.net", ReadOnly: true},
		})
	exchangedDirID := createDirectory(t, eA, driveRootID, "Exchanged", env.acmeToken)
	noteID := createNoteInDriveForDriveToken(t, eA, driveID, exchangedDirID, "Exchanged note", env.acmeToken)
	acceptSharedDrive(t, env.acme, env.betty, "Betty", env.tsA.URL, env.tsB.URL, driveID)
	acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, driveID)

	// The member of the cozy-to-cozy sharing is not a member of the drive.
	peer := newDriveTokenPeer(t)
	c2c := createCozyToCozySharingInDrive(t, eA, env.acme, exchangedDirID, env.acmeToken)
	c2c.Members[1].Email = "peer@example.net"
	c2c.Members[1].Instance = peer.URL
	c2c.Credentials[0].XorKey = sharing.MakeXorKey()
	c2c.Credentials[0].AccessToken = &auth.AccessToken{AccessToken: "token-for-the-peer"}
	require.NoError(t, couchdb.UpdateDoc(env.acme, c2c))
	c2cCode, err := c2c.GetInteractCode(env.acme, &c2c.Members[1], 1)
	require.NoError(t, err)

	notePath := "/sharings/drives/" + driveID + "/notes/" + noteID + "/open"
	sourceOf := func(t *testing.T, code string) (string, string) {
		t.Helper()
		attrs := eA.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+code).
			Expect().Status(http.StatusOK).
			JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
			Object().Path("$.data.attributes").Object()
		return attrs.Value("type").String().Raw(), attrs.Value("source_id").String().Raw()
	}
	checkDriveCodes := func(t *testing.T) {
		t.Helper()
		typ, source := sourceOf(t, sharecodeForDriveToken(t, eB, notePath, env.bettyToken))
		assert.Equal(t, "share-interact", typ)
		assert.Equal(t, consts.Sharings+"/"+driveID, source)
		typ, source = sourceOf(t, sharecodeForDriveToken(t, eD, notePath, env.daveToken))
		assert.Equal(t, "share-preview", typ)
		assert.Equal(t, consts.Sharings+"/"+driveID, source)
		assert.Empty(t, peer.takeCalls(), "a file of a drive is opened on the instance of its owner")
	}
	openWithC2CCode := func(t *testing.T) string {
		t.Helper()
		return sharecodeForDriveToken(t, eA, "/notes/"+noteID+"/open", c2cCode)
	}

	t.Run("FileCreatedOnTheOwner", func(t *testing.T) {
		checkDriveCodes(t)
		assert.Equal(t, c2cCode, openWithC2CCode(t))
		assert.Empty(t, peer.takeCalls())
	})

	t.Run("FileCreatedOnTheOtherMemberOfTheCozyToCozySharing", func(t *testing.T) {
		markFileCreatedOn(t, env.acme, noteID, peer.URL)
		checkDriveCodes(t)
		assert.Equal(t, "code-from-peer", openWithC2CCode(t))
		assert.Len(t, peer.takeCalls(), 1)
	})
}
