package sharings_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/web/editor"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/notes"
	"github.com/cozy/cozy-stack/web/office"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newOpenClient serves the direct /office, /notes and /editor routes of the
// given instance, that the shared test environment does not mount for all its
// instances.
func newOpenClient(t *testing.T, inst *instance.Instance) *httpexpect.Expect {
	t.Helper()
	config.GetConfig().Office = map[string]config.Office{
		config.DefaultInstanceContext: {OnlyOfficeURL: "https://documentserver.example", OutboxSecret: "outbox"},
	}
	handler := echo.New()
	handler.HTTPErrorHandler = errors.ErrorHandler
	withInstance := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("instance", inst)
			return next(c)
		}
	}
	office.Routes(handler.Group("/office", withInstance))
	notes.Routes(handler.Group("/notes", withInstance))
	editor.Routes(handler.Group("/editor", withInstance))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return newSharingExpect(t, ts.URL)
}

func createNote(t *testing.T, e *httpexpect.Expect, path, dirID, title, token string) string {
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

func createNoteInDrive(t *testing.T, e *httpexpect.Expect, driveID, dirID, title, token string) string {
	t.Helper()
	return createNote(t, e, "/sharings/drives/"+driveID+"/notes", dirID, title, token)
}

func openSharecode(t *testing.T, e *httpexpect.Expect, path, token string) string {
	t.Helper()
	return e.GET(path).
		WithHeader("Authorization", "Bearer "+token).
		Expect().Status(http.StatusOK).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.attributes.sharecode").String().NotEmpty().Raw()
}

func permissionTypeOf(t *testing.T, e *httpexpect.Expect, code string) string {
	t.Helper()
	return e.GET("/permissions/self").
		WithHeader("Authorization", "Bearer "+code).
		Expect().Status(http.StatusOK).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.attributes.type").String().Raw()
}

// createCozyToCozySharing creates a regular sharing on the folder through the
// API, and activates it like Betty accepting it would do. It does not depend on
// a real acceptance: the only needed effects are the active flag and the
// io.cozy.shared documents, that Sharing.Setup creates with InitialIndex.
func createCozyToCozySharing(t *testing.T, e *httpexpect.Expect, inst *instance.Instance, dirID, token string) *sharing.Sharing {
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

// A SharingID query parameter does not widen what a token can open: this test
// checks that a file outside of the sharing stays refused with or without it.
func TestOpenOfficeOutsideOfSharingWithSharingIDParam(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, eD := env.createClients(t)
	eOffice := newOpenClient(t, env.acme)

	// Dave is a read-only member of a drive.
	roDriveID, roDriveRootID, _ := createSharedDrive(t, DriveCreationMethodFromFolder,
		env.acme, env.acmeToken, env.tsA.URL, "Open SharingID drive", "drive",
		[]RecipientInfo{{Name: "Dave", Email: "dave@example.net", ReadOnly: true}})
	roNoteID := createNoteInDrive(t, eA, roDriveID, roDriveRootID, "Read-only note", env.acmeToken)
	acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, roDriveID)

	// Betty is a writer of the first drive of the environment.
	rwNoteID := createNoteInDrive(t, eA, env.firstSharingID, env.meetingsDirID, "Writer note", env.acmeToken)

	// Betty is a writer of a cozy-to-cozy sharing.
	exchangedDirID := createRootDirectory(t, eA, "Exchanged", env.acmeToken)
	c2c := createCozyToCozySharing(t, eA, env.acme, exchangedDirID, env.acmeToken)
	c2cCode, err := c2c.GetInteractCode(env.acme, &c2c.Members[1], 1)
	require.NoError(t, err)

	// Documents of the owner, outside of every sharing.
	privateDirID := createRootDirectory(t, eA, "Private", env.acmeToken)
	pdfID := createFileWithMime(t, eA, privateDirID, "secret.pdf", env.acmeToken, "application/pdf")
	txtID := createFile(t, eA, privateDirID, "secret.txt", env.acmeToken)

	// The share-preview code in the browser of Dave after opening a note.
	previewCode := openSharecode(t, eD, "/sharings/drives/"+roDriveID+"/notes/"+roNoteID+"/open", env.daveToken)
	require.Equal(t, "share-preview", permissionTypeOf(t, eA, previewCode))

	// The share-interact code in the browser of Betty after opening a note.
	writerCode := openSharecode(t, eB, "/sharings/drives/"+env.firstSharingID+"/notes/"+rwNoteID+"/open", env.bettyToken)
	require.Equal(t, "share-interact", permissionTypeOf(t, eA, writerCode))

	// The share-interact code that the instance of Dave has as its drive token.
	daveSharing, err := sharing.FindSharing(env.dave, roDriveID)
	require.NoError(t, err)
	driveToken := daveSharing.Credentials[0].DriveToken
	require.NotEmpty(t, driveToken)

	cases := []struct {
		name, code, sharingID string
	}{
		{"DriveWriterCodeFromRegularOpen", writerCode, env.firstSharingID},
		{"DriveReadOnlyMemberDriveToken", driveToken, roDriveID},
		{"CozyToCozyMemberInteractCode", c2cCode, c2c.ID()},
	}
	for _, tc := range cases {
		for fileName, fileID := range map[string]string{"pdf": pdfID, "txt": txtID} {
			t.Run(tc.name+"/"+fileName, func(t *testing.T) {
				// Without the parameter, the VFS check rejects the request.
				eOffice.GET("/office/"+fileID+"/open").
					WithHeader("Authorization", "Bearer "+tc.code).
					Expect().Status(http.StatusForbidden)

				res := eOffice.GET("/office/"+fileID+"/open").
					WithQuery("SharingID", tc.sharingID).
					WithHeader("Authorization", "Bearer "+tc.code).
					Expect()
				status := res.Raw().StatusCode
				assert.Equal(t, http.StatusForbidden, status, "the file is outside of the sharing of the code")
				if status != http.StatusOK {
					return
				}

				doc := res.JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
					Object().Path("$.data.attributes.onlyoffice.document").Object()
				downloadURL, err := url.Parse(doc.Value("url").String().Raw())
				require.NoError(t, err)
				content := eA.GET(downloadURL.Path).Expect().Status(http.StatusOK).Body().Raw()
				t.Errorf("a download link was returned for a file outside of the sharing: %q, with edit=%v",
					content, doc.Path("$.permissions.edit").Raw())
			})
		}
	}

	t.Run("ReadOnlyMemberShortPreviewCodeIsRejected", func(t *testing.T) {
		eOffice.GET("/office/"+pdfID+"/open").
			WithQuery("SharingID", roDriveID).
			WithHeader("Authorization", "Bearer "+previewCode).
			Expect().Status(http.StatusForbidden)
	})
}

// The notes and editor routes make the same check as the office one.
func TestOpenNoteAndEditorOutsideOfSharingWithSharingIDParam(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)
	eOpen := newOpenClient(t, env.acme)

	rwNoteID := createNoteInDrive(t, eA, env.firstSharingID, env.meetingsDirID, "Writer note", env.acmeToken)
	writerCode := openSharecode(t, eB, "/sharings/drives/"+env.firstSharingID+"/notes/"+rwNoteID+"/open", env.bettyToken)
	require.Equal(t, "share-interact", permissionTypeOf(t, eA, writerCode))

	privateDirID := createRootDirectory(t, eA, "Private", env.acmeToken)
	noteID := createNote(t, eOpen, "/notes", privateDirID, "Private note", env.acmeToken)
	txtID := createFile(t, eA, privateDirID, "secret.txt", env.acmeToken)

	for name, path := range map[string]string{
		"Note":   "/notes/" + noteID + "/open",
		"Editor": "/editor/" + txtID + "/open",
	} {
		t.Run(name, func(t *testing.T) {
			eOpen.GET(path).
				WithHeader("Authorization", "Bearer "+writerCode).
				Expect().Status(http.StatusForbidden)
			status := eOpen.GET(path).
				WithQuery("SharingID", env.firstSharingID).
				WithHeader("Authorization", "Bearer "+writerCode).
				Expect().Raw().StatusCode
			assert.Equal(t, http.StatusForbidden, status, "the file is outside of the sharing of the code")
		})
	}
}

// openPeer stands for the instance of the other member of a sharing, where a
// file created on this instance is opened.
type openPeer struct {
	*httptest.Server
	mu    sync.Mutex
	calls []*http.Request
}

func newOpenPeer(t *testing.T) *openPeer {
	t.Helper()
	peer := &openPeer{}
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
func (p *openPeer) takeCalls() []*http.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	calls := p.calls
	p.calls = nil
	return calls
}

// createExchangedSharing creates a cozy-to-cozy sharing as seen from an
// accepted member, with the io.cozy.shared documents of the folder, and returns
// it with the token of the OAuth client that its peer uses on this instance.
func createExchangedSharing(t *testing.T, inst *instance.Instance, owner bool, dirID, localURL, peerURL string) (*sharing.Sharing, string) {
	t.Helper()
	ownerURL, recipientURL := localURL, peerURL
	if !owner {
		ownerURL, recipientURL = peerURL, localURL
	}
	s := &sharing.Sharing{
		Active:      true,
		Owner:       owner,
		Description: "Exchanged",
		Members: []sharing.Member{
			{Status: sharing.MemberStatusOwner, Name: "Owner", Email: "owner@example.net", Instance: ownerURL},
			{Status: sharing.MemberStatusReady, Name: "Recipient", Email: "recipient@example.net", Instance: recipientURL},
		},
		Credentials: []sharing.Credentials{{
			XorKey:      sharing.MakeXorKey(),
			AccessToken: &auth.AccessToken{AccessToken: "token-for-the-peer"},
		}},
		Rules: []sharing.Rule{{
			Title:   "Exchanged",
			DocType: consts.Files,
			Values:  []string{dirID},
			Add:     sharing.ActionRuleSync,
			Update:  sharing.ActionRuleSync,
			Remove:  sharing.ActionRuleSync,
		}},
	}
	require.NoError(t, couchdb.CreateDoc(inst, s))

	peerMember := &s.Members[1]
	if !owner {
		peerMember = &s.Members[0]
	}
	cli, err := sharing.CreateOAuthClient(inst, peerMember)
	require.NoError(t, err)
	token, err := sharing.CreateAccessToken(inst, cli, s.ID(), permission.ALL)
	require.NoError(t, err)
	s.Credentials[0].InboundClientID = cli.ClientID
	require.NoError(t, couchdb.UpdateDoc(inst, s))

	require.NoError(t, couchdb.EnsureDBExist(inst, consts.Shared))
	require.NoError(t, s.InitialIndex(inst, s.Rules[0], 0))
	return s, token.AccessToken
}

// openableFile is a file with one of the routes that can open it.
type openableFile struct {
	route, id string
}

func (f openableFile) path() string {
	return "/" + f.route + "/" + f.id + "/open"
}

func createOpenableFiles(t *testing.T, e, eOpen *httpexpect.Expect, dirID, prefix, token string) []openableFile {
	t.Helper()
	return []openableFile{
		{"office", createFileWithMime(t, e, dirID, prefix+".pdf", token, "application/pdf")},
		{"notes", createNote(t, eOpen, "/notes", dirID, prefix+" note", token)},
		{"editor", createFile(t, e, dirID, prefix+".txt", token)},
	}
}

// markCreatedOn makes the file look like it has been created on another
// instance of the sharing, that is the instance where it should be opened.
func markCreatedOn(t *testing.T, inst *instance.Instance, files []openableFile, instanceURL string) {
	t.Helper()
	for _, f := range files {
		file, err := inst.VFS().FileByID(f.id)
		require.NoError(t, err)
		require.NotNil(t, file.CozyMetadata)
		file.CozyMetadata.CreatedOn = instanceURL
		require.NoError(t, couchdb.UpdateDoc(inst, file))
	}
}

// tokensUnrelatedToSharing returns tokens of the instance that have no rights
// on the files of the given folder and that are not from a member of its
// sharing.
func tokensUnrelatedToSharing(t *testing.T, inst *instance.Instance, owner bool, otherDirID, localURL, peerURL string) map[string]string {
	t.Helper()
	other, otherClientToken := createExchangedSharing(t, inst, owner, otherDirID, localURL, peerURL)
	interactCode, err := other.GetInteractCode(inst, &other.Members[1], 1)
	require.NoError(t, err)

	linkCode, err := inst.CreateShareCode("link", "")
	require.NoError(t, err)
	_, err = permission.CreateShareSet(inst, nil, consts.Apps+"/drive", map[string]string{"link": linkCode}, nil,
		permission.Permission{Permissions: permission.Set{{
			Type:   consts.Files,
			Verbs:  permission.ALL,
			Values: []string{otherDirID},
		}}}, nil, true)
	require.NoError(t, err)

	cli := oauth.Client{
		RedirectURIs: []string{"http://localhost/oauth/callback"},
		ClientName:   "unrelated-client",
		SoftwareID:   "github.com/cozy/cozy-stack/testing/open",
	}
	require.Nil(t, cli.Create(inst, oauth.NotPending))
	oauthToken, err := inst.MakeJWT(consts.AccessTokenAudience, cli.ClientID, consts.Contacts, "", time.Now())
	require.NoError(t, err)

	return map[string]string{
		"InteractCodeOfAnotherSharing": interactCode,
		"ClientOfAnotherSharing":       otherClientToken,
		"ShareByLinkCode":              linkCode,
		"AppWithoutRights":             generateAppToken(inst, "unrelated-app", consts.Contacts),
		"UnrelatedOAuthClient":         oauthToken,
	}
}

// The SharingID query parameter is for the instance of a member that opens a
// file of their sharing with its OAuth client: another token naming this
// sharing gets what it would get without the parameter.
func TestOpenSharedFileWithSharingIDParamNeedsClientOfSharing(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)

	cases := []struct {
		name            string
		inst            *instance.Instance
		e               *httpexpect.Expect
		token, localURL string
		owner           bool
		// The index of the requesting member on the instance of the file
		peerMemberIndex string
	}{
		{"OnOwner", env.acme, eA, env.acmeToken, env.tsA.URL, true, "1"},
		{"OnRecipient", env.betty, eB, env.bettyToken, env.tsB.URL, false, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peer := newOpenPeer(t)
			eOpen := newOpenClient(t, tc.inst)

			dirID := createRootDirectory(t, tc.e, "Exchanged", tc.token)
			otherDirID := createRootDirectory(t, tc.e, "Other exchanged", tc.token)
			localFiles := createOpenableFiles(t, tc.e, eOpen, dirID, "local", tc.token)
			peerFiles := createOpenableFiles(t, tc.e, eOpen, dirID, "peer", tc.token)
			markCreatedOn(t, tc.inst, peerFiles, peer.URL)

			s, clientToken := createExchangedSharing(t, tc.inst, tc.owner, dirID, tc.localURL, peer.URL)
			unrelated := tokensUnrelatedToSharing(t, tc.inst, tc.owner, otherDirID, tc.localURL, peer.URL)

			open := func(f openableFile, code string) *httpexpect.Response {
				return eOpen.GET(f.path()).
					WithQuery("SharingID", s.ID()).
					WithQuery("MemberIndex", "1").
					WithHeader("Authorization", "Bearer "+code).
					Expect()
			}

			for tokenName, code := range unrelated {
				for where, files := range map[string][]openableFile{"Local": localFiles, "Peer": peerFiles} {
					for _, f := range files {
						t.Run(tokenName+"/"+where+"/"+f.route, func(t *testing.T) {
							status := open(f, code).Raw().StatusCode
							assert.Equal(t, http.StatusForbidden, status)
							assert.Empty(t, peer.takeCalls(), "the request must not reach the instance of the peer")
						})
					}
				}
			}

			for _, f := range localFiles {
				t.Run("ClientOfTheSharing/Local/"+f.route, func(t *testing.T) {
					expected, err := s.GetInteractCode(tc.inst, &s.Members[1], 1)
					require.NoError(t, err)
					open(f, clientToken).Status(http.StatusOK).
						JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
						Object().Path("$.data.attributes.sharecode").String().IsEqual(expected)
					assert.Empty(t, peer.takeCalls())
				})
			}

			for _, f := range peerFiles {
				t.Run("ClientOfTheSharing/Peer/"+f.route, func(t *testing.T) {
					open(f, clientToken).Status(http.StatusOK).
						JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
						Object().Path("$.data.attributes.sharecode").String().IsEqual("code-from-peer")
					calls := peer.takeCalls()
					require.Len(t, calls, 1)
					assert.Equal(t, s.ID(), calls[0].URL.Query().Get("SharingID"))
					assert.Equal(t, tc.peerMemberIndex, calls[0].URL.Query().Get("MemberIndex"))
					assert.Equal(t, "false", calls[0].URL.Query().Get("ReadOnly"))
					assert.Equal(t, "Bearer token-for-the-peer", calls[0].Header.Get("Authorization"))
				})
			}

			for name, verbs := range map[string]permission.VerbSet{
				"ReadOnlyFileLink": permission.Verbs(permission.GET),
				"WritableFileLink": permission.ALL,
			} {
				t.Run(name, func(t *testing.T) {
					code, err := tc.inst.CreateShareCode(name, "")
					require.NoError(t, err)
					rule := permission.Rule{Type: consts.Files, Verbs: verbs}
					for _, f := range peerFiles {
						rule.Values = append(rule.Values, f.id)
					}
					_, err = permission.CreateShareSet(tc.inst, nil, consts.Apps+"/drive", map[string]string{"link": code}, nil,
						permission.Permission{Permissions: permission.Set{rule}}, nil, true)
					require.NoError(t, err)
					for _, f := range peerFiles {
						open(f, code).Status(http.StatusForbidden)
					}
					assert.Empty(t, peer.takeCalls())
				})
			}

			t.Run("ReadOnlyOAuth", func(t *testing.T) {
				client := oauth.Client{
					ClientName: "reader", SoftwareID: "github.com/cozy/cozy-stack/testing/reader",
					RedirectURIs: []string{"http://localhost/oauth/callback"},
				}
				require.Nil(t, client.Create(tc.inst, oauth.NotPending))
				token, err := tc.inst.MakeJWT(consts.AccessTokenAudience, client.ClientID, consts.Files+":GET", "", time.Now())
				require.NoError(t, err)
				for _, f := range peerFiles {
					open(f, token).Status(http.StatusForbidden)
				}
				assert.Empty(t, peer.takeCalls())
				for _, f := range localFiles {
					obj := open(f, token).Status(http.StatusOK).
						JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).Object()
					if f.route == "office" {
						obj.Path("$.data.attributes.onlyoffice.document.permissions.edit").Boolean().IsFalse()
					}
				}
			})

			t.Run("ReadOnlyMemberOpen", func(t *testing.T) {
				if !tc.owner {
					// Recipient preview documents previously omitted the owner.
					preview, err := s.CreatePreviewPermissions(tc.inst)
					require.NoError(t, err)
					delete(preview.Codes, s.Members[0].Email)
					delete(preview.ShortCodes, s.Members[0].Email)
					preview.Permissions[0].Values = []string{otherDirID}
					require.NoError(t, couchdb.UpdateDoc(tc.inst, preview))
				}
				for _, f := range localFiles {
					code := eOpen.GET(f.path()).WithQuery("SharingID", s.ID()).WithQuery("ReadOnly", "true").
						WithHeader("Authorization", "Bearer "+clientToken).Expect().Status(http.StatusOK).
						JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
						Object().Path("$.data.attributes.sharecode").String().NotEmpty().Raw()
					_, perm, err := permission.GetTokenAndPermissionsFromShortcode(tc.inst, code)
					require.NoError(t, err)
					require.Equal(t, permission.TypeSharePreview, perm.Type)
					require.Len(t, perm.Permissions, 1)
					assert.Equal(t, permission.Verbs(permission.GET), perm.Permissions[0].Verbs)
					assert.Equal(t, []string{dirID}, perm.Permissions[0].Values)
				}
				for _, f := range peerFiles {
					eOpen.GET(f.path()).WithQuery("SharingID", s.ID()).WithQuery("ReadOnly", "true").
						WithHeader("Authorization", "Bearer "+clientToken).Expect().Status(http.StatusOK)
					calls := peer.takeCalls()
					require.Len(t, calls, 1)
					assert.Equal(t, "true", calls[0].URL.Query().Get("ReadOnly"))
					assert.Equal(t, tc.peerMemberIndex, calls[0].URL.Query().Get("MemberIndex"))
				}
			})

			// A token with rights on the file opens it like without the parameter.
			appToken := generateAppToken(tc.inst, "files-app", consts.Files)
			for _, f := range localFiles {
				t.Run("AppWithRights/Local/"+f.route, func(t *testing.T) {
					open(f, appToken).Status(http.StatusOK)
					assert.Empty(t, peer.takeCalls())
				})
			}
			for _, f := range peerFiles {
				t.Run("AppWithRights/Peer/"+f.route, func(t *testing.T) {
					open(f, appToken).Status(http.StatusOK)
					require.Len(t, peer.takeCalls(), 1)
				})
			}
		})
	}
}
