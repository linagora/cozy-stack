package sharings_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/client"
	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/pkg/shortcut"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/notes"
	"github.com/cozy/cozy-stack/web/sharings"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DriveCreationMethod represents how a shared drive is created in tests.
type DriveCreationMethod string

const (
	// DriveCreationMethodLegacy uses POST /sharings/ with drive:true and manual rules.
	DriveCreationMethodLegacy DriveCreationMethod = "legacy"
	// DriveCreationMethodFromFolder uses POST /sharings/drives with folder_id.
	DriveCreationMethodFromFolder DriveCreationMethod = "from_folder"
)

// RecipientInfo describes a recipient for a shared drive.
type RecipientInfo struct {
	Name     string
	Email    string
	ReadOnly bool
}

// createDirectory creates a directory with the given name in the specified parent directory
// and returns the directory ID.
func createDirectory(t *testing.T, client *httpexpect.Expect, parentDirID, name, token string) string {
	t.Helper()
	dirID := client.POST("/files/"+parentDirID).
		WithQuery("Name", name).
		WithQuery("Type", "directory").
		WithHeader("Authorization", "Bearer "+token).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object().Path("$.data.id").String().NotEmpty().Raw()
	require.NotEmpty(t, dirID, "Error creating directory")
	return dirID
}

// createRootDirectory creates a directory with the given name at the root level
// and returns the directory ID.
func createRootDirectory(t *testing.T, client *httpexpect.Expect, name, token string) string {
	return createDirectory(t, client, "", name, token)
}

// createFile creates a file with the given name in the specified parent directory
// and returns the file ID.
func createFile(t *testing.T, client *httpexpect.Expect, parentDirID, name, token string) string {
	return createFileWithMime(t, client, parentDirID, name, token, "text/plain")
}

func createFileWithMime(t *testing.T, client *httpexpect.Expect, parentDirID, name, token, mime string) string {
	t.Helper()
	fileID := client.POST("/files/"+parentDirID).
		WithQuery("Name", name).
		WithQuery("Type", "file").
		WithHeader("Content-Type", mime).
		WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
		WithHeader("Authorization", "Bearer "+token).
		WithBytes([]byte("foo")).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object().Path("$.data.id").String().NotEmpty().Raw()
	require.NotEmpty(t, fileID, "Error creation of the file")
	return fileID
}

func createShortcut(t *testing.T, client *httpexpect.Expect, parentDirID, name, token, targetURL string) string {
	t.Helper()

	fileID := client.POST("/files/"+parentDirID).
		WithQuery("Name", name).
		WithQuery("Type", "file").
		WithHeader("Content-Type", "application/octet-stream").
		WithHeader("Authorization", "Bearer "+token).
		WithBytes(shortcut.Generate(targetURL)).
		Expect().Status(http.StatusCreated).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object().Path("$.data.id").String().NotEmpty().Raw()
	require.NotEmpty(t, fileID, "Error creation of the shortcut")
	return fileID
}

// verifyFileMove verifies that a file was moved successfully by checking its attributes.
func verifyFileMove(t *testing.T, inst *instance.Instance, fileID, expectedName, expectedDirID string, expectedContent string) {
	t.Helper()
	fs := inst.VFS()

	fileDoc, err := fs.FileByID(fileID)
	require.NoError(t, err)
	require.Equal(t, expectedName, fileDoc.DocName)
	require.Equal(t, expectedDirID, fileDoc.DirID)
	require.Equal(t, int64(len(expectedContent)), fileDoc.ByteSize)

	fileHandle, err := fs.OpenFile(fileDoc)
	require.NoError(t, err)
	defer fileHandle.Close()

	content, err := io.ReadAll(fileHandle)
	require.NoError(t, err)
	require.Equal(t, expectedContent, string(content))
}

// verifyFileDeleted verifies that a file was fully deleted from the source instance: both the
// CouchDB metadata record and the physical file content in storage. doc must be fetched before
// the deletion so its path is available after the CouchDB record is gone.
func verifyFileDeleted(t *testing.T, inst *instance.Instance, doc *vfs.FileDoc) {
	t.Helper()
	_, err := inst.VFS().FileByID(doc.DocID)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))

	_, err = inst.VFS().OpenFile(doc)
	require.Error(t, err, "expected physical file to be deleted from storage, but it is still readable")
}

// verifyNodeDeleted verifies that a node (file or directory) was deleted from the source instance.
func verifyNodeDeleted(t *testing.T, inst *instance.Instance, id string) {
	t.Helper()
	if _, err := inst.VFS().FileByID(id); err == nil {
		require.Fail(t, "expected file to be deleted, but it still exists")
	} else if os.IsNotExist(err) {
		return
	}
	if _, err := inst.VFS().DirByID(id); err == nil {
		require.Fail(t, "expected directory to be deleted, but it still exists")
	} else {
		require.True(t, os.IsNotExist(err))
	}
}

// DefaultRecipients returns the default Betty+Dave recipients for tests.
func DefaultRecipients() []RecipientInfo {
	return []RecipientInfo{
		{Name: "Betty", Email: "betty@example.net", ReadOnly: false},
		{Name: "Dave", Email: "dave@example.net", ReadOnly: true},
	}
}

// createSharedDrive creates a shared drive using the specified method.
// If recipients is nil, it uses DefaultRecipients().
// It returns the created sharing ID and the shared root directory ID.
func createSharedDrive(
	t *testing.T,
	method DriveCreationMethod,
	inst *instance.Instance,
	appToken string,
	tsURL string,
	driveName string,
	description string,
	recipients []RecipientInfo,
) (
	sharingID string,
	productID string,
	discovery string,
) {
	t.Helper()

	e := httpexpect.Default(t, tsURL)

	// Use default recipients if none provided
	if recipients == nil {
		recipients = DefaultRecipients()
	}

	// Create contacts and build relationship arrays
	var rwRecipientRefs, roRecipientRefs []string
	for _, r := range recipients {
		c := createContact(t, inst, r.Name, r.Email)
		require.NotNil(t, c)
		ref := `{"id": "` + c.ID() + `", "type": "` + c.DocType() + `"}`
		if r.ReadOnly {
			roRecipientRefs = append(roRecipientRefs, ref)
		} else {
			rwRecipientRefs = append(rwRecipientRefs, ref)
		}
	}

	// Create the directory that will be shared
	productID = e.POST("/files/").
		WithQuery("Name", driveName).
		WithQuery("Type", "directory").
		WithHeader("Authorization", "Bearer "+appToken).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object().Path("$.data.id").String().NotEmpty().Raw()

	var obj *httpexpect.Object

	// Build relationships JSON
	rwRecipientsJSON := "[" + strings.Join(rwRecipientRefs, ",") + "]"
	roRecipientsJSON := "[" + strings.Join(roRecipientRefs, ",") + "]"

	switch method {
	case DriveCreationMethodLegacy:
		// Use POST /sharings/ with drive:true and manual rules
		obj = e.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+appToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
				"data": {
					"type": "` + consts.Sharings + `",
					"attributes": {
						"description": "` + description + `",
						"drive": true,
						"rules": [{
							"title": "` + driveName + `",
							"doctype": "` + consts.Files + `",
							"values": ["` + productID + `"]
						}]
					},
					"relationships": {
						"recipients": {
							"data": ` + rwRecipientsJSON + `
						},
						"read_only_recipients": {
							"data": ` + roRecipientsJSON + `
						}
					}
				}
			}`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

	case DriveCreationMethodFromFolder:
		// Use POST /sharings/drives with folder_id
		obj = e.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+appToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
				"data": {
					"type": "` + consts.Sharings + `",
					"attributes": {
						"description": "` + description + `",
						"folder_id": "` + productID + `"
					},
					"relationships": {
						"recipients": {
							"data": ` + rwRecipientsJSON + `
						},
						"read_only_recipients": {
							"data": ` + roRecipientsJSON + `
						}
					}
				}
			}`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()
	}

	sharingID = obj.Value("data").Object().Value("id").String().NotEmpty().Raw()

	// Extract invitation link
	publicName, _ := inst.SettingsPublicName()
	sentDescription, disco := extractInvitationLink(t, inst, sharingID, publicName, "")
	assert.Equal(t, sentDescription, description)
	assert.Contains(t, disco, "/discovery?state=")
	discovery = disco

	return
}

func createFileRootSharedDrive(
	t *testing.T,
	inst *instance.Instance,
	appToken string,
	tsURL string,
	fileID string,
	description string,
	recipients []RecipientInfo,
) (
	sharingID string,
	discovery string,
) {
	t.Helper()

	e := httpexpect.Default(t, tsURL)

	if recipients == nil {
		recipients = DefaultRecipients()
	}

	var rwRecipientRefs, roRecipientRefs []string
	for _, r := range recipients {
		c := createContact(t, inst, r.Name, r.Email)
		require.NotNil(t, c)
		ref := `{"id": "` + c.ID() + `", "type": "` + c.DocType() + `"}`
		if r.ReadOnly {
			roRecipientRefs = append(roRecipientRefs, ref)
		} else {
			rwRecipientRefs = append(rwRecipientRefs, ref)
		}
	}

	rwRecipientsJSON := "[" + strings.Join(rwRecipientRefs, ",") + "]"
	roRecipientsJSON := "[" + strings.Join(roRecipientRefs, ",") + "]"

	obj := e.POST("/sharings/drives").
		WithHeader("Authorization", "Bearer "+appToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{
			"data": {
				"type": "` + consts.Sharings + `",
				"attributes": {
					"description": "` + description + `",
					"file_id": "` + fileID + `"
				},
				"relationships": {
					"recipients": {
						"data": ` + rwRecipientsJSON + `
					},
					"read_only_recipients": {
						"data": ` + roRecipientsJSON + `
					}
				}
			}
		}`)).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	sharingID = obj.Value("data").Object().Value("id").String().NotEmpty().Raw()

	publicName, _ := inst.SettingsPublicName()
	sentDescription, disco := extractInvitationLink(t, inst, sharingID, publicName, "")
	assert.Equal(t, sentDescription, description)
	assert.Contains(t, disco, "/discovery?state=")
	discovery = disco

	return
}

func requireNoDirSharingReference(t *testing.T, inst *instance.Instance, dirID, sharingID string) {
	t.Helper()

	dir, err := inst.VFS().DirByID(dirID)
	require.NoError(t, err)
	require.NotContains(t, dir.ReferencedBy, couchdb.DocReference{
		ID:   sharingID,
		Type: consts.Sharings,
	})
}

func requireNoFileSharingReference(t *testing.T, inst *instance.Instance, fileID, sharingID string) {
	t.Helper()

	file, err := inst.VFS().FileByID(fileID)
	require.NoError(t, err)
	require.NotContains(t, file.ReferencedBy, couchdb.DocReference{
		ID:   sharingID,
		Type: consts.Sharings,
	})
}

func makeAddRecipientsPayload(t *testing.T, sharingID, relationshipName string, contactIDs ...string) []byte {
	t.Helper()

	refs := make([]map[string]interface{}, 0, len(contactIDs))
	for _, contactID := range contactIDs {
		refs = append(refs, map[string]interface{}{
			"id":   contactID,
			"type": consts.Contacts,
		})
	}

	return mustJSON(t, map[string]interface{}{
		"data": map[string]interface{}{
			"type": consts.Sharings,
			"id":   sharingID,
			"relationships": map[string]interface{}{
				relationshipName: map[string]interface{}{
					"data": refs,
				},
			},
		},
	})
}

func createAcceptedSharedDriveForRecipient(
	t *testing.T,
	env *sharedDrivesEnv,
	recipient RecipientInfo,
	recipientInst *instance.Instance,
	recipientURL string,
) string {
	t.Helper()

	sharingID, _, _ := createSharedDrive(
		t,
		DriveCreationMethodLegacy,
		env.acme,
		env.acmeToken,
		env.tsA.URL,
		testify(t, recipient.Name+"_delegated_drive"),
		"Drive for delegated recipient addition tests",
		[]RecipientInfo{recipient},
	)

	acceptSharedDrive(t, env.acme, recipientInst, recipient.Name, env.tsA.URL, recipientURL, sharingID)
	return sharingID
}

func findSharingMemberByEmail(t *testing.T, inst *instance.Instance, sharingID, email string) sharing.Member {
	t.Helper()

	s, err := sharing.FindSharing(inst, sharingID)
	require.NoError(t, err)

	for _, member := range s.Members {
		if member.Email == email {
			return member
		}
	}

	require.FailNowf(t, "member not found", "email %s not found in sharing %s", email, sharingID)
	return sharing.Member{}
}

func findSharingMemberByName(t *testing.T, inst *instance.Instance, sharingID, name string) sharing.Member {
	t.Helper()

	s, err := sharing.FindSharing(inst, sharingID)
	require.NoError(t, err)

	for _, member := range s.Members {
		if member.Name == name {
			return member
		}
	}

	require.FailNowf(t, "member not found", "name %s not found in sharing %s", name, sharingID)
	return sharing.Member{}
}

func sharingInvitationRecipientNames(t *testing.T, inst *instance.Instance, sharingID string) []string {
	t.Helper()

	req := couchdb.AllDocsRequest{}
	var jobs []job.Job
	require.NoError(t, couchdb.GetAllDocs(inst, consts.Jobs, &req, &jobs))

	expectedPath := "/sharings/" + sharingID + "/discovery"
	var names []string
	for _, j := range jobs {
		if j.WorkerType != "sendmail" {
			continue
		}

		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(j.Message, &msg))
		if msg["template_name"] != "sharing_request" {
			continue
		}
		values, ok := msg["template_values"].(map[string]interface{})
		require.True(t, ok)
		link, ok := values["SharingLink"].(string)
		require.True(t, ok)
		u, err := url.Parse(link)
		require.NoError(t, err)
		if u.Path != expectedPath {
			continue
		}
		require.NotEmpty(t, u.Query().Get("state"))
		name, ok := msg["recipient_name"].(string)
		require.True(t, ok)
		names = append(names, name)
	}
	return names
}

func findSharingMemberIndexByEmail(t *testing.T, inst *instance.Instance, sharingID, email string) int {
	t.Helper()

	s, err := sharing.FindSharing(inst, sharingID)
	require.NoError(t, err)

	for i, member := range s.Members {
		if member.Email == email {
			return i
		}
	}

	require.FailNowf(t, "member not found", "email %s not found in sharing %s", email, sharingID)
	return -1
}

func requireOwnerMemberState(
	t *testing.T,
	owner *instance.Instance,
	sharingID string,
	email string,
	status string,
	readOnly bool,
) {
	t.Helper()

	member := findSharingMemberByEmail(t, owner, sharingID, email)
	require.Equal(t, status, member.Status)
	require.Equal(t, readOnly, member.ReadOnly)
}

type driveAutoAcceptEnv struct {
	ownerInstance     *instance.Instance
	recipientInstance *instance.Instance
	ownerAppToken     string
	ownerURL          string
	recipientURL      string
	eOwner            *httpexpect.Expect
	eRecipient        *httpexpect.Expect
}

func newSharingExpect(t *testing.T, baseURL string) *httpexpect.Expect {
	t.Helper()

	return httpexpect.WithConfig(httpexpect.Config{
		BaseURL:  baseURL,
		Reporter: httpexpect.NewRequireReporter(t),
		Printers: []httpexpect.Printer{httpexpect.NewCompactPrinter(t)},
	})
}

func mergeRouteHandlers(
	base map[string]func(*echo.Group),
	extra map[string]func(*echo.Group),
) map[string]func(*echo.Group) {
	if len(extra) == 0 {
		return base
	}

	routes := make(map[string]func(*echo.Group), len(base)+len(extra))
	for path, handler := range base {
		routes[path] = handler
	}
	for path, handler := range extra {
		routes[path] = handler
	}
	return routes
}

func newDriveOwnerTestServer(
	t *testing.T,
	setupName string,
	email string,
	publicName string,
	render echo.Renderer,
	extraRoutes map[string]func(*echo.Group),
) (*instance.Instance, string, *httptest.Server) {
	t.Helper()

	return newDriveOwnerTestServerWithOptions(
		t,
		setupName,
		&lifecycle.Options{
			Email:      email,
			PublicName: publicName,
		},
		render,
		extraRoutes,
	)
}

func newDriveOwnerTestServerWithOptions(
	t *testing.T,
	setupName string,
	ownerOptions *lifecycle.Options,
	render echo.Renderer,
	extraRoutes map[string]func(*echo.Group),
) (*instance.Instance, string, *httptest.Server) {
	t.Helper()

	setup := testutils.NewSetup(t, setupName)
	options := lifecycle.Options{}
	if ownerOptions != nil {
		options = *ownerOptions
	}
	if options.Email == "" {
		options.Email = "acme@example.net"
	}
	if options.PublicName == "" {
		options.PublicName = "ACME"
	}

	inst := setup.GetTestInstance(&options)
	token := generateAppToken(inst, "drive", consts.Files)

	routes := mergeRouteHandlers(map[string]func(*echo.Group){
		"/files":    files.Routes,
		"/sharings": sharings.Routes,
	}, extraRoutes)
	ts := setup.GetTestServerMultipleRoutes(routes)
	ts.Config.Handler.(*echo.Echo).Renderer = render
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	return inst, token, ts
}

func newDriveRecipientTestServer(
	t *testing.T,
	setupName string,
	email string,
	publicName string,
	render echo.Renderer,
	extraRoutes map[string]func(*echo.Group),
) (*instance.Instance, string, *httptest.Server) {
	t.Helper()

	setup := testutils.NewSetup(t, setupName)
	inst := setup.GetTestInstance(&lifecycle.Options{
		Email:         email,
		PublicName:    publicName,
		Passphrase:    "MyPassphrase",
		KdfIterations: 5000,
		Key:           "xxx",
	})
	token := generateAppToken(inst, "drive", consts.Files)

	routes := mergeRouteHandlers(map[string]func(*echo.Group){
		"/auth": func(g *echo.Group) {
			g.Use(middlewares.LoadSession)
			auth.Routes(g)
		},
		"/files":    files.Routes,
		"/sharings": sharings.Routes,
	}, extraRoutes)
	ts := setup.GetTestServerMultipleRoutes(routes)
	ts.Config.Handler.(*echo.Echo).Renderer = render
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	return inst, token, ts
}

func setupDriveAutoAcceptEnv(t *testing.T, ownerAnswerDelay time.Duration) *driveAutoAcceptEnv {
	t.Helper()

	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	cfg := config.GetConfig()
	cfg.Assets = "../../assets"
	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()), "Could not init dynamic FS")

	prevContexts := cfg.Contexts
	cfg.Contexts = map[string]interface{}{
		config.DefaultInstanceContext: map[string]interface{}{
			"sharing": map[string]interface{}{
				"auto_accept_trusted": true,
				"trusted_domains":     []interface{}{"cozy.local", "example.com"},
			},
		},
	}
	t.Cleanup(func() {
		cfg.Contexts = prevContexts
	})

	testName := strings.ReplaceAll(t.Name(), "/", "_")
	ownerInstance, ownerAppToken, tsOwner := newDriveOwnerTestServer(
		t,
		testName+"_owner",
		"owner@example.com",
		"Owner",
		render,
		map[string]func(*echo.Group){
			"/sharings": func(g *echo.Group) {
				if ownerAnswerDelay > 0 {
					g.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
						return func(c echo.Context) error {
							if c.Request().Method == http.MethodPost && strings.HasSuffix(c.Request().URL.Path, "/answer") {
								time.Sleep(ownerAnswerDelay)
							}
							return next(c)
						}
					})
				}
				sharings.Routes(g)
			},
		},
	)

	recipientInstance, _, tsRecipient := newDriveRecipientTestServer(
		t,
		testName+"_recipient",
		"recipient@example.com",
		"Recipient",
		render,
		nil,
	)

	return &driveAutoAcceptEnv{
		ownerInstance:     ownerInstance,
		recipientInstance: recipientInstance,
		ownerAppToken:     ownerAppToken,
		ownerURL:          tsOwner.URL,
		recipientURL:      tsRecipient.URL,
		eOwner:            newSharingExpect(t, tsOwner.URL),
		eRecipient:        newSharingExpect(t, tsRecipient.URL),
	}
}

func createDirectRecipientDriveSharing(
	t *testing.T,
	ownerInstance *instance.Instance,
	eOwner *httpexpect.Expect,
	ownerAppToken string,
	recipientName string,
	recipientEmail string,
	recipientURL string,
	driveName string,
	description string,
) string {
	t.Helper()

	sharedDirID := createRootDirectory(t, eOwner, driveName, ownerAppToken)
	recipientContact := createContact(t, ownerInstance, recipientName, recipientEmail)
	recipientContact.M["cozy"] = []interface{}{
		map[string]interface{}{"url": recipientURL, "primary": true},
	}
	require.NoError(t, couchdb.UpdateDoc(ownerInstance, recipientContact))

	payload := fmt.Sprintf(`{
		"data": {
			"type": "%s",
			"attributes": {
				"description": "%s",
				"drive": true,
				"rules": [{
					"title": "%s",
					"doctype": "%s",
					"values": ["%s"]
				}]
			},
			"relationships": {
				"recipients": {
					"data": [{
						"id": "%s",
						"type": "%s"
					}]
				}
			}
		}
	}`, consts.Sharings, description, driveName, consts.Files, sharedDirID, recipientContact.ID(), recipientContact.DocType())

	return eOwner.POST("/sharings/").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(payload)).
		Expect().
		Status(http.StatusCreated).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object().
		Path("$.data.id").String().NotEmpty().Raw()
}

func loginSharingRecipient(t *testing.T, eRecipient *httpexpect.Expect) string {
	t.Helper()

	token := eRecipient.GET("/auth/login").
		Expect().Status(http.StatusOK).
		Cookie("_csrf").Value().NotEmpty().Raw()
	eRecipient.POST("/auth/login").
		WithCookie("_csrf", token).
		WithFormField("passphrase", "MyPassphrase").
		WithFormField("csrf_token", token).
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().Status(http.StatusSeeOther).
		Header("Location").Contains("home")
	return token
}

func prepareSharingAuthorizeLink(
	t *testing.T,
	ownerInstance *instance.Instance,
	recipientName string,
	sharingID string,
	eOwner *httpexpect.Expect,
	recipientURL string,
) string {
	t.Helper()

	ownerPublicName, _ := ownerInstance.SettingsPublicName()
	_, discoveryLink := extractInvitationLink(t, ownerInstance, sharingID, ownerPublicName, recipientName)
	return submitSharingDiscovery(t, eOwner, discoveryLink, recipientURL)
}

func submitSharingDiscovery(
	t *testing.T,
	eOwner *httpexpect.Expect,
	discoveryLink string,
	recipientURL string,
) string {
	t.Helper()

	discoveryURL, err := url.Parse(discoveryLink)
	require.NoError(t, err)
	state := discoveryURL.Query().Get("state")
	require.NotEmpty(t, state)

	eOwner.GET(discoveryURL.Path).
		WithQuery("state", state).
		Expect().Status(http.StatusOK).
		HasContentType("text/html", "utf-8").
		Body().
		Contains("Connect to your Twake").
		Contains(`<input type="hidden" name="state" value="` + state)

	redirectHeader := eOwner.POST(discoveryURL.Path).
		WithFormField("state", state).
		WithFormField("slug", recipientURL).
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().Status(http.StatusFound).Header("Location")

	redirectHeader.Contains(recipientURL)
	redirectHeader.Contains("/auth/authorize/sharing")
	return redirectHeader.NotEmpty().Raw()
}

func assertSharedDriveRedirectLocation(t *testing.T, location, sharingID string) {
	t.Helper()

	u, err := url.Parse(location)
	require.NoError(t, err)

	folderPrefix := "/shareddrive/" + sharingID + "/"
	filePrefix := "/sharings/shareddrive/" + sharingID + "/file/"
	switch {
	case strings.HasPrefix(u.Fragment, folderPrefix):
		require.NotEmpty(t, strings.TrimPrefix(u.Fragment, folderPrefix), "folder redirect should include a root folder id")
	case strings.HasPrefix(u.Fragment, filePrefix):
		require.NotEmpty(t, strings.TrimPrefix(u.Fragment, filePrefix), "file redirect should include a root file id")
	default:
		require.Failf(
			t,
			"unexpected shared drive redirect",
			"expected Location %q fragment to start with %q or %q",
			location,
			folderPrefix,
			filePrefix,
		)
	}
}

func assertInvalidSharingUnavailableErrorPage(t *testing.T, body *httpexpect.String) {
	t.Helper()

	body.Contains("This item is no longer available")
	body.Contains("The file or folder was deleted or your access was removed.")
	body.Contains("Contact the owner for more information.")
	body.NotContains("unexpected error")
	body.NotContains("Sorry, an unexpected error occurred.")
	body.NotContains("Unqualified error")
	body.NotContains("sharing was not found or has been revoked")
	body.NotContains(`"error":`)
	body.NotContains(`{"error"`)
}

func submitSharingAuthorize(
	t *testing.T,
	eRecipient *httpexpect.Expect,
	authorizeLink string,
	sharingID string,
	csrfToken string,
) {
	t.Helper()

	authorizeURL, err := url.Parse(authorizeLink)
	require.NoError(t, err)
	state := authorizeURL.Query().Get("state")
	require.NotEmpty(t, state)

	location := eRecipient.POST(authorizeURL.Path).
		WithFormField("sharing_id", sharingID).
		WithFormField("state", state).
		WithFormField("csrf_token", csrfToken).
		WithFormField("synchronize", true).
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().Status(http.StatusSeeOther).
		Header("Location").
		Raw()
	assertSharedDriveRedirectLocation(t, location, sharingID)
}

func openSharingAuthorize(
	t *testing.T,
	eRecipient *httpexpect.Expect,
	authorizeLink string,
	sharingID string,
) {
	t.Helper()

	authorizeURL, err := url.Parse(authorizeLink)
	require.NoError(t, err)
	state := authorizeURL.Query().Get("state")
	require.NotEmpty(t, state)

	location := eRecipient.GET(authorizeURL.Path).
		WithQuery("sharing_id", sharingID).
		WithQuery("state", state).
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().
		Status(http.StatusSeeOther).
		Header("Location").
		Raw()
	assertSharedDriveRedirectLocation(t, location, sharingID)
}

func waitForSharingOnRecipient(t *testing.T, recipientInstance *instance.Instance, sharingID string) *sharing.Sharing {
	t.Helper()

	var recipientSharing *sharing.Sharing
	require.Eventually(t, func() bool {
		var err error
		recipientSharing, err = sharing.FindSharing(recipientInstance, sharingID)
		return err == nil
	}, 10*time.Second, 250*time.Millisecond, "Drive sharing request not created on recipient side")
	return recipientSharing
}

func waitForSharingOnRecipientWithOwnerURL(t *testing.T, recipientInstance *instance.Instance, sharingID string, ownerURL string) *sharing.Sharing {
	t.Helper()

	var recipientSharing *sharing.Sharing
	require.Eventually(t, func() bool {
		s, err := sharing.FindSharing(recipientInstance, sharingID)
		if err != nil || len(s.Members) == 0 {
			return false
		}
		s.Members[0].Instance = ownerURL
		if err := couchdb.UpdateDoc(recipientInstance, s); err != nil {
			return false
		}
		recipientSharing = s
		return true
	}, 10*time.Second, 25*time.Millisecond, "Drive sharing request not created on recipient side")
	return recipientSharing
}

func hasAutoAcceptJobForSharing(recipientInstance *instance.Instance, sharingID string) bool {
	req := couchdb.AllDocsRequest{}
	var jobs []job.Job
	if err := couchdb.GetAllDocs(recipientInstance, consts.Jobs, &req, &jobs); err != nil {
		return false
	}
	for _, j := range jobs {
		if j.WorkerType == "share-autoaccept" && bytes.Contains(j.Message, []byte(sharingID)) {
			return true
		}
	}
	return false
}

func waitForAutoAcceptJobForSharing(t *testing.T, recipientInstance *instance.Instance, sharingID string) {
	t.Helper()

	require.Eventually(t, func() bool {
		return hasAutoAcceptJobForSharing(recipientInstance, sharingID)
	}, 5*time.Second, 25*time.Millisecond, "Auto-accept job was not created for the sharing")
}

func assertNoAutoAcceptJobForSharing(t *testing.T, recipientInstance *instance.Instance, sharingID string) {
	t.Helper()

	require.Never(t, func() bool {
		return hasAutoAcceptJobForSharing(recipientInstance, sharingID)
	}, 500*time.Millisecond, 25*time.Millisecond, "Auto-accept job should not be created for interactive sharing")
}

func waitForDriveSharingReadyOnOwner(
	t *testing.T,
	eOwner *httpexpect.Expect,
	ownerAppToken string,
	sharingID string,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		ownerSharing := eOwner.GET("/sharings/"+sharingID).
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().
			Status(http.StatusOK).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := ownerSharing.Value("data").Object().Value("attributes").Object()
		members := attrs.Value("members").Array()
		if members.Length().Raw() < 2 {
			return false
		}
		return attrs.Value("active").Boolean().Raw() &&
			members.Value(1).Object().Value("status").String().Raw() == sharing.MemberStatusReady
	}, 10*time.Second, 250*time.Millisecond, "Drive sharing did not become active on owner side")
}

func waitForDriveSharingActiveOnRecipient(t *testing.T, recipientInstance *instance.Instance, sharingID string) {
	t.Helper()

	require.Eventually(t, func() bool {
		recipientSharing, err := sharing.FindSharing(recipientInstance, sharingID)
		return err == nil && recipientSharing.Active
	}, 10*time.Second, 250*time.Millisecond, "Drive sharing did not become active on recipient side")
}

// acceptSharedDrive performs the acceptance flow on the recipient side.
// It extracts the discovery link for the specific recipient and completes the acceptance flow.
func acceptSharedDrive(
	t *testing.T,
	ownerInstance *instance.Instance,
	recipientInstance *instance.Instance,
	recipientName string,
	tsAURL string,
	tsRecipientURL string,
	sharingID string,
) {
	t.Helper()
	eA := newSharingExpect(t, tsAURL)
	eR := newSharingExpect(t, tsRecipientURL)

	token := loginSharingRecipient(t, eR)
	authorizeLink := prepareSharingAuthorizeLink(t, ownerInstance, recipientName, sharingID, eA, tsRecipientURL)

	// Ensure the owner instance URL is set for this specific sharing
	FakeOwnerInstanceForSharing(t, recipientInstance, tsAURL, sharingID)
	submitSharingAuthorize(t, eR, authorizeLink, sharingID, token)
}

// acceptSharedDriveForBetty is kept for convenience and delegates to acceptSharedDrive.
func acceptSharedDriveForBetty(
	t *testing.T,
	ownerInstance *instance.Instance,
	bettyInstance *instance.Instance,
	tsAURL string,
	tsBURL string,
	sharingID string,
) {
	acceptSharedDrive(t, ownerInstance, bettyInstance, "Betty", tsAURL, tsBURL, sharingID)
}

func mockAcmeClient(uA *url.URL) func(u *url.URL, bearer string) *client.Client {
	return func(u *url.URL, bearer string) *client.Client {
		tr := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, uA.Host)
			},
		}
		c := &client.Client{
			Scheme:    uA.Scheme,
			Addr:      uA.Host,
			Domain:    u.Hostname(),
			Transport: tr,
		}
		if bearer != "" {
			c.Authorizer = &request.BearerAuthorizer{Token: bearer}
		}
		return c
	}
}

// runCoreSharedDrivesTests runs core shared drive tests with a specific creation method.
func runCoreSharedDrivesTests(t *testing.T, method DriveCreationMethod) {
	var sharingID, productID, meetingsID, checklistID string

	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	cfg := config.GetConfig()
	cfg.Assets = "../../assets"
	cfg.Contexts = map[string]interface{}{
		config.DefaultInstanceContext: map[string]interface{}{
			"sharing": map[string]interface{}{
				"auto_accept_trusted_contacts": true,
				"auto_accept_trusted":          true,
				"trusted_domains":              []interface{}{"cozy.local", "example.com"},
			},
		},
	}
	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	// Prepare the owner instance - replace "/" with "_" for valid domain names
	testName := strings.ReplaceAll(t.Name(), "/", "_")
	ownerSetup := testutils.NewSetup(t, testName+"_owner")
	ownerInstance := ownerSetup.GetTestInstance(&lifecycle.Options{
		Email:      "owner@example.net",
		PublicName: "Owner",
	})
	ownerAppToken := generateAppToken(ownerInstance, "drive", "io.cozy.files")
	tsOwner := ownerSetup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/files":    files.Routes,
		"/notes":    notes.Routes,
		"/sharings": sharings.Routes,
	})
	tsOwner.Config.Handler.(*echo.Echo).Renderer = render
	tsOwner.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsOwner.Close)

	// Prepare Betty's instance (recipient)
	bettySetup := testutils.NewSetup(t, testName+"_betty")
	bettyInstance := bettySetup.GetTestInstance(&lifecycle.Options{
		Email:         "betty@example.net",
		PublicName:    "Betty",
		Passphrase:    "MyPassphrase",
		KdfIterations: 5000,
		Key:           "xxx",
	})
	bettyAppToken := generateAppToken(bettyInstance, "drive", consts.Files)
	tsBetty := bettySetup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/auth": func(g *echo.Group) {
			g.Use(middlewares.LoadSession)
			auth.Routes(g)
		},
		"/files":    files.Routes,
		"/sharings": sharings.Routes,
	})
	tsBetty.Config.Handler.(*echo.Echo).Renderer = render
	tsBetty.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsBetty.Close)

	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()), "Could not init dynamic FS")

	eOwner := httpexpect.Default(t, tsOwner.URL)
	eBetty := httpexpect.Default(t, tsBetty.URL)

	t.Run("CreateSharedDrive", func(t *testing.T) {
		sid, dirID, _ := createSharedDrive(t, method, ownerInstance, ownerAppToken, tsOwner.URL, "Product team", "Drive for the product team", nil)
		sharingID, productID = sid, dirID

		// Create additional files for subsequent tests
		meetingsID = createDirectory(t, eOwner, productID, "Meetings", ownerAppToken)
		checklistID = createFile(t, eOwner, meetingsID, "Checklist.txt", ownerAppToken)
	})

	t.Run("ListSharedDrives", func(t *testing.T) {
		obj := eOwner.GET("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().IsEqual(1)

		sharingObj := data.Value(0).Object()
		sharingObj.Value("type").IsEqual("io.cozy.sharings")
		sharingObj.Value("id").String().NotEmpty()

		attrs := sharingObj.Value("attributes").Object()
		attrs.Value("description").IsEqual("Drive for the product team")
		attrs.Value("app_slug").IsEqual("drive")
		attrs.Value("owner").IsEqual(true)
		attrs.Value("drive").IsEqual(true)
		attrs.NotContainsKey("org_drive")

		members := attrs.Value("members").Array()
		members.Length().IsEqual(3)

		owner := members.Value(0).Object()
		owner.Value("status").IsEqual("owner")
		owner.Value("public_name").IsEqual("Owner")

		rules := attrs.Value("rules").Array()
		rules.Length().IsEqual(1)
		rule := rules.Value(0).Object()
		rule.Value("title").IsEqual("Product team")
		rule.Value("doctype").IsEqual("io.cozy.files")
		rule.Value("values").Array().Value(0).IsEqual(productID)
	})

	t.Run("AcceptSharedDrive", func(t *testing.T) {
		acceptSharedDrive(t, ownerInstance, bettyInstance, "Betty", tsOwner.URL, tsBetty.URL, sharingID)

		// Verify that owner's contact was created and marked as trusted on Betty's side
		var s sharing.Sharing
		require.NoError(t, couchdb.GetDoc(bettyInstance, consts.Sharings, sharingID, &s))
		require.NotEmpty(t, s.Members[0].Email, "Owner should have an email")

		ownerContact, err := contact.FindByEmail(bettyInstance, s.Members[0].Email)
		require.NoError(t, err, "Owner's contact should exist on Betty's instance after accepting")
		require.True(t, ownerContact.IsTrusted(), "Owner's contact should be marked as trusted on Betty's side after she accepted the sharing")
	})

	t.Run("OwnerSeesRecipientReadyStatus", func(t *testing.T) {
		recipientObj := eBetty.GET("/sharings/"+sharingID).
			WithHeader("Authorization", "Bearer "+bettyAppToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		recipientAttrs := recipientObj.Value("data").Object().Value("attributes").Object()
		recipientMembers := recipientAttrs.Value("members").Array()
		recipientMembers.Length().IsEqual(3)
		recipientMembers.Value(1).Object().Value("email").IsEqual("betty@example.net")
		recipientMembers.Value(1).Object().Value("status").IsEqual("ready")

		ownerObj := eOwner.GET("/sharings/"+sharingID).
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		ownerAttrs := ownerObj.Value("data").Object().Value("attributes").Object()
		ownerMembers := ownerAttrs.Value("members").Array()
		ownerMembers.Length().IsEqual(3)
		ownerMembers.Value(1).Object().Value("email").IsEqual("betty@example.net")
		ownerMembers.Value(1).Object().Value("status").IsEqual("ready")
	})

	t.Run("AccessDirOrFile", func(t *testing.T) {
		t.Run("HEAD", func(t *testing.T) {
			// HEAD request on non-existing file should return 404
			eBetty.HEAD("/sharings/drives/"+sharingID+"/nonexistent").
				WithHeader("Authorization", "Bearer "+bettyAppToken).
				Expect().Status(404)

			// HEAD request on directory should return 200
			eBetty.HEAD("/sharings/drives/"+sharingID+"/"+productID).
				WithHeader("Authorization", "Bearer "+bettyAppToken).
				Expect().Status(200)

			// HEAD request on file should return 200
			eBetty.HEAD("/sharings/drives/"+sharingID+"/"+meetingsID).
				WithHeader("Authorization", "Bearer "+bettyAppToken).
				Expect().Status(200)

			// HEAD request without authentication should fail
			eBetty.HEAD("/sharings/drives/" + sharingID + "/" + checklistID).
				Expect().Status(401)

			// HEAD request with wrong sharing ID should fail
			eBetty.HEAD("/sharings/drives/wrong-id/"+checklistID).
				WithHeader("Authorization", "Bearer "+bettyAppToken).
				Expect().Status(404)
		})

		t.Run("GET", func(t *testing.T) {
			// GET request on non-existing file should return 404
			eBetty.GET("/sharings/drives/"+sharingID+"/nonexistent").
				WithHeader("Authorization", "Bearer "+bettyAppToken).
				Expect().Status(404)

			// GET request on directory should return 200 and directory data
			obj := eBetty.GET("/sharings/drives/"+sharingID+"/"+meetingsID).
				WithHeader("Authorization", "Bearer "+bettyAppToken).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			data.Value("type").String().IsEqual("io.cozy.files")
			data.Value("id").String().IsEqual(meetingsID)
			attrs := data.Value("attributes").Object()
			attrs.Value("type").String().IsEqual("directory")
			attrs.Value("name").String().IsEqual("Meetings")
			attrs.Value("path").String().IsEqual("/Product team/Meetings")
			attrs.Value("driveId").String().IsEqual(sharingID)

			contents := data.Path("$.relationships.contents.data").Array()
			contents.Length().IsEqual(1)
			fileRef := contents.Value(0).Object()
			fileRef.Value("type").String().IsEqual("io.cozy.files")
			fileRef.Value("id").String().IsEqual(checklistID)

			// GET request on file should return 200 and file data
			obj = eBetty.GET("/sharings/drives/"+sharingID+"/"+checklistID).
				WithHeader("Authorization", "Bearer "+bettyAppToken).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data = obj.Value("data").Object()
			data.Value("type").String().IsEqual("io.cozy.files")
			data.Value("id").String().IsEqual(checklistID)
			attrs = data.Value("attributes").Object()
			attrs.Value("type").String().IsEqual("file")
			attrs.Value("name").String().IsEqual("Checklist.txt")
			attrs.Value("mime").String().IsEqual("text/plain")
			attrs.Value("size").String().IsEqual("3")

			// GET request without authentication should fail
			eBetty.GET("/sharings/drives/" + sharingID + "/" + meetingsID).
				Expect().Status(401)

			// GET request with wrong sharing ID should fail
			eBetty.GET("/sharings/drives/wrong-id/"+meetingsID).
				WithHeader("Authorization", "Bearer "+bettyAppToken).
				Expect().Status(404)
		})
	})
}

// TestCoreSharedDrivesWithBothMethods runs core shared drive tests with both
// the legacy API (POST /sharings/ with drive:true) and the new API
// (POST /sharings/drives with folder_id).
func TestCoreSharedDrivesWithBothMethods(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	methods := []DriveCreationMethod{
		DriveCreationMethodLegacy,
		DriveCreationMethodFromFolder,
	}

	for _, method := range methods {
		t.Run(string(method), func(t *testing.T) {
			runCoreSharedDrivesTests(t, method)
		})
	}
}

func TestCreateDriveFromFolder(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	config.GetConfig().Assets = "../../assets"
	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	// Setup test instance
	setup := testutils.NewSetup(t, t.Name()+"_owner")
	ownerInstance := setup.GetTestInstance(&lifecycle.Options{
		Email:      "owner@example.net",
		PublicName: "Owner",
	})
	ownerAppToken := generateAppToken(ownerInstance, "drive", consts.Files)
	tsOwner := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/files":    files.Routes,
		"/sharings": sharings.Routes,
	})
	tsOwner.Config.Handler.(*echo.Echo).Renderer = render
	tsOwner.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsOwner.Close)

	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()), "Could not init dynamic FS")

	eOwner := httpexpect.Default(t, tsOwner.URL)

	t.Run("CreateDriveFromUnsharedFolder", func(t *testing.T) {
		// Create a directory
		dirID := createRootDirectory(t, eOwner, "ProjectDocs", ownerAppToken)

		// Create a contact for the recipient
		recipientContact := createContact(t, ownerInstance, "Alice", "alice@example.net")

		// Create shared drive from that folder using the new endpoint
		obj := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "Project Documents",
						"folder_id": "%s"
					},
					"relationships": {
						"recipients": {
							"data": [{"id": "%s", "type": "%s"}]
						}
					}
				}
			}`, consts.Sharings, dirID, recipientContact.ID(), recipientContact.DocType()))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Verify sharing was created with correct attributes
		data := obj.Value("data").Object()
		data.Value("id").String().NotEmpty()
		attrs := data.Value("attributes").Object()
		attrs.Value("drive").Boolean().IsTrue()
		attrs.NotContainsKey("org_drive")
		attrs.Value("description").String().IsEqual("Project Documents")

		// Verify the rule was created correctly
		rules := attrs.Value("rules").Array()
		rules.Length().IsEqual(1)
		rule := rules.Value(0).Object()
		rule.Value("title").String().IsEqual("ProjectDocs")
		rule.Value("doctype").String().IsEqual(consts.Files)
		rule.Value("values").Array().Value(0).String().IsEqual(dirID)
	})

	t.Run("CreateDriveWithoutRecipients", func(t *testing.T) {
		// Create a directory
		dirID := createRootDirectory(t, eOwner, "NoRecipientsFolder", ownerAppToken)

		// Create shared drive without recipients (should succeed)
		obj := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "Drive without recipients",
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, dirID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Verify sharing was created
		data := obj.Value("data").Object()
		data.Value("id").String().NotEmpty()
		attrs := data.Value("attributes").Object()
		attrs.Value("drive").Boolean().IsTrue()
		attrs.NotContainsKey("org_drive")
	})

	t.Run("CreateDriveFromName", func(t *testing.T) {
		recipientContact := createContact(t, ownerInstance, "Eve", "eve@example.net")

		obj := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"name": "BrandNewDrive"
					},
					"relationships": {
						"recipients": {
							"data": [{"id": "%s", "type": "%s"}]
						}
					}
				}
			}`, consts.Sharings, recipientContact.ID(), recipientContact.DocType()))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		attrs := data.Value("attributes").Object()
		attrs.Value("drive").Boolean().IsTrue()
		attrs.Value("description").String().IsEqual("BrandNewDrive")

		rules := attrs.Value("rules").Array()
		rules.Length().IsEqual(1)
		rule := rules.Value(0).Object()
		rule.Value("title").String().IsEqual("BrandNewDrive")
		createdDirID := rule.Value("values").Array().Value(0).String().NotEmpty().Raw()

		createdDir, err := ownerInstance.VFS().DirByID(createdDirID)
		require.NoError(t, err)
		require.Equal(t, consts.SharedDrivesDirID, createdDir.DirID)

		sharedDrivesDir, err := ownerInstance.EnsureSharedDrivesDir()
		require.NoError(t, err)
		require.Equal(t, sharedDrivesDir.ID(), createdDir.DirID)
	})

	t.Run("FailOnMissingFolderIDFileIDAndName", func(t *testing.T) {
		resp := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "No folder ID"
					}
				}
			}`, consts.Sharings))).
			Expect().Status(400)

		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("folder_id, file_id or name is required")
	})

	t.Run("FailOnBothFolderIDAndName", func(t *testing.T) {
		dirID := createRootDirectory(t, eOwner, "AmbiguousDriveFolder", ownerAppToken)

		resp := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s",
						"name": "AmbiguousDrive"
					}
				}
			}`, consts.Sharings, dirID))).
			Expect().Status(400)

		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("mutually exclusive")
	})

	t.Run("FailOnBothFolderIDAndFileID", func(t *testing.T) {
		dirID := createRootDirectory(t, eOwner, "AmbiguousFileDriveFolder", ownerAppToken)
		fileID := createFile(t, eOwner, "", "AmbiguousFileDrive.txt", ownerAppToken)

		resp := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s",
						"file_id": "%s"
					}
				}
			}`, consts.Sharings, dirID, fileID))).
			Expect().Status(400)

		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("mutually exclusive")
	})

	t.Run("FailOnDuplicateNameInSharedDrives", func(t *testing.T) {
		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"name": "DuplicateNamedDrive"
					}
				}
			}`, consts.Sharings))).
			Expect().Status(201)

		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"name": "DuplicateNamedDrive"
					}
				}
			}`, consts.Sharings))).
			Expect().Status(409)
	})

	t.Run("FailOnNonexistentFolder", func(t *testing.T) {
		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "nonexistent-folder-id"
					}
				}
			}`, consts.Sharings))).
			Expect().Status(404)
	})

	t.Run("FailOnNonexistentFile", func(t *testing.T) {
		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"file_id": "nonexistent-file-id"
					}
				}
			}`, consts.Sharings))).
			Expect().Status(404)
	})

	t.Run("AllowOnFileWithFolderID", func(t *testing.T) {
		fileID := createFile(t, eOwner, "", "test.txt", ownerAppToken)

		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, fileID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.drive_root_type").String().IsEqual("file")
	})

	t.Run("AllowOnDirectoryWithFileID", func(t *testing.T) {
		dirID := createRootDirectory(t, eOwner, "NotAFileDriveRoot", ownerAppToken)

		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"file_id": "%s"
					}
				}
			}`, consts.Sharings, dirID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.drive_root_type").String().IsEqual("directory")
	})

	t.Run("FailOnSystemFolder", func(t *testing.T) {
		// Try to share the root directory
		resp := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
			"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, consts.RootDirID))).
			Expect().Status(400)

		// Verify error message
		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("Cannot share system folder")

		// Try to share the trash directory
		resp = eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
			"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, consts.TrashDirID))).
			Expect().Status(400)

		// Verify error message
		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("Cannot share system folder")

		// Try to share the shared-drives directory
		resp = eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
			"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, consts.SharedDrivesDirID))).
			Expect().Status(400)

		// Verify error message
		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("Cannot share system folder")
	})

	t.Run("FailOnAlreadySharedFolder", func(t *testing.T) {
		// Create a directory and share it first
		dirID := createRootDirectory(t, eOwner, "AlreadyShared", ownerAppToken)

		recipientContact := createContact(t, ownerInstance, "Bob", "bob@example.net")

		// Create first sharing
		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					},
					"relationships": {
						"recipients": {
							"data": [{"id": "%s", "type": "%s"}]
						}
					}
				}
			}`, consts.Sharings, dirID, recipientContact.ID(), recipientContact.DocType()))).
			Expect().Status(201)

		// Try to create another drive from the same folder
		resp := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, dirID))).
			Expect().Status(409)

		// Verify error message
		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("already has an existing sharing")
	})

	t.Run("FailOnFolderInsideSharing", func(t *testing.T) {
		// Create a parent directory and share it
		parentID := createRootDirectory(t, eOwner, "ParentShared", ownerAppToken)

		// Create a subdirectory inside
		childID := createDirectory(t, eOwner, parentID, "ChildFolder", ownerAppToken)

		recipientContact := createContact(t, ownerInstance, "Carol", "carol@example.net")

		// Share the parent
		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					},
					"relationships": {
						"recipients": {
							"data": [{"id": "%s", "type": "%s"}]
						}
					}
				}
			}`, consts.Sharings, parentID, recipientContact.ID(), recipientContact.DocType()))).
			Expect().Status(201)

		// Try to share the child folder (should fail because it's inside a shared folder)
		resp := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, childID))).
			Expect().Status(409)

		// Verify error message
		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("already has an existing sharing")
	})

	t.Run("FailOnAlreadySharedFile", func(t *testing.T) {
		fileID := createFile(t, eOwner, "", "AlreadySharedFile.txt", ownerAppToken)

		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"file_id": "%s"
					}
				}
			}`, consts.Sharings, fileID))).
			Expect().Status(201)

		resp := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"file_id": "%s"
					}
				}
			}`, consts.Sharings, fileID))).
			Expect().Status(409)

		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("already has an existing sharing")
	})

	t.Run("FailOnFileInsideSharedFolder", func(t *testing.T) {
		parentID := createRootDirectory(t, eOwner, "ParentSharedForFile", ownerAppToken)
		fileID := createFile(t, eOwner, parentID, "NestedDriveFile.txt", ownerAppToken)

		recipientContact := createContact(t, ownerInstance, "Eve", "eve@example.net")

		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					},
					"relationships": {
						"recipients": {
							"data": [{"id": "%s", "type": "%s"}]
						}
					}
				}
			}`, consts.Sharings, parentID, recipientContact.ID(), recipientContact.DocType()))).
			Expect().Status(201)

		resp := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"file_id": "%s"
					}
				}
			}`, consts.Sharings, fileID))).
			Expect().Status(409)

		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("already has an existing sharing")
	})

	t.Run("FailOnFolderContainingSharedChild", func(t *testing.T) {
		// Create a parent directory
		parentID := createRootDirectory(t, eOwner, "ParentWithSharedChild", ownerAppToken)

		// Create a subdirectory inside
		childID := createDirectory(t, eOwner, parentID, "SharedChildFolder", ownerAppToken)

		recipientContact := createContact(t, ownerInstance, "Dan", "dan@example.net")

		// Share the child folder first
		eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					},
					"relationships": {
						"recipients": {
							"data": [{"id": "%s", "type": "%s"}]
						}
					}
				}
			}`, consts.Sharings, childID, recipientContact.ID(), recipientContact.DocType()))).
			Expect().Status(201)

		// Try to share the parent folder (should fail because it contains a shared folder)
		resp := eOwner.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+ownerAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, parentID))).
			Expect().Status(409)

		// Verify error message
		resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.errors[0].detail").String().
			Contains("already has an existing sharing")
	})
}

func TestDriveAutoAcceptTrusted(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupDriveAutoAcceptEnv(t, 0)
	sharingID := createDirectRecipientDriveSharing(
		t,
		env.ownerInstance,
		env.eOwner,
		env.ownerAppToken,
		"Recipient",
		"recipient@example.com",
		env.recipientURL,
		"Shared Drive",
		"Auto-accept test drive",
	)

	waitForSharingOnRecipientWithOwnerURL(t, env.recipientInstance, sharingID, env.ownerURL)
	waitForAutoAcceptJobForSharing(t, env.recipientInstance, sharingID)
	waitForDriveSharingReadyOnOwner(t, env.eOwner, env.ownerAppToken, sharingID)
	waitForDriveSharingActiveOnRecipient(t, env.recipientInstance, sharingID)

	loginSharingRecipient(t, env.eRecipient)
	recipientSharing, err := sharing.FindSharing(env.recipientInstance, sharingID)
	require.NoError(t, err)
	require.NotEmpty(t, recipientSharing.Credentials)
	state := recipientSharing.Credentials[0].State
	require.NotEmpty(t, state)

	location := env.eRecipient.GET("/auth/authorize/sharing").
		WithQuery("sharing_id", sharingID).
		WithQuery("state", state).
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().
		Status(http.StatusSeeOther).
		Header("Location").
		Raw()
	assertSharedDriveRedirectLocation(t, location, sharingID)
}

func TestRevokedSharedDriveInvitationAuthorizeShowsErrorPage(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eOwner, eRecipient, _ := env.createClients(t)
	sharingID := createDirectRecipientDriveSharing(
		t,
		env.acme,
		eOwner,
		env.acmeToken,
		"Betty revoked",
		"betty@example.net",
		env.tsB.URL,
		"Revoked invitation drive",
		"Revoked invitation drive",
	)

	recipientSharing := waitForSharingOnRecipient(t, env.betty, sharingID)
	require.NotEmpty(t, recipientSharing.Credentials)
	state := recipientSharing.Credentials[0].State
	require.NotEmpty(t, state)

	csrfToken := loginSharingRecipient(t, eRecipient)
	eOwner.DELETE("/sharings/"+sharingID+"/recipients/1").
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		Expect().Status(http.StatusNoContent)

	require.Eventually(t, func() bool {
		revokedSharing, err := sharing.FindSharing(env.betty, sharingID)
		return err == nil &&
			len(revokedSharing.Credentials) == 0 &&
			len(revokedSharing.Members) > 1 &&
			revokedSharing.Members[1].Status == sharing.MemberStatusRevoked
	}, 5*time.Second, 100*time.Millisecond, "recipient sharing should be marked as revoked")

	FakeOwnerInstanceForSharing(t, env.betty, env.tsA.URL, sharingID)
	body := eRecipient.GET("/auth/authorize/sharing").
		WithQuery("sharing_id", sharingID).
		WithQuery("state", state).
		Expect().
		Status(http.StatusBadRequest).
		HasContentType("text/html", "utf-8").
		Body()
	assertInvalidSharingUnavailableErrorPage(t, body)

	body = eRecipient.POST("/auth/authorize/sharing").
		WithFormField("sharing_id", sharingID).
		WithFormField("state", state).
		WithFormField("csrf_token", csrfToken).
		Expect().
		Status(http.StatusBadRequest).
		HasContentType("text/html", "utf-8").
		Body()
	assertInvalidSharingUnavailableErrorPage(t, body)
}

func TestInvalidSharingDiscoveryShowsErrorPage(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eOwner, _, _ := env.createClients(t)

	body := eOwner.GET("/sharings/missing-sharing/discovery").
		WithQuery("state", "missing-state").
		Expect().
		Status(http.StatusBadRequest).
		HasContentType("text/html", "utf-8").
		Body()
	assertInvalidSharingUnavailableErrorPage(t, body)
}

func TestSendAnswerIsIdempotentAfterStaleConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eOwner, _, _ := env.createClients(t)
	sharingID := createDirectRecipientDriveSharing(
		t,
		env.acme,
		eOwner,
		env.acmeToken,
		"Betty direct",
		"betty@example.net",
		env.tsB.URL,
		"Idempotent send answer",
		"Idempotent send answer",
	)

	waitForSharingOnRecipient(t, env.betty, sharingID)

	FakeOwnerInstanceForSharing(t, env.betty, env.tsA.URL, sharingID)

	firstAttempt, err := sharing.FindSharing(env.betty, sharingID)
	require.NoError(t, err)
	secondAttempt, err := sharing.FindSharing(env.betty, sharingID)
	require.NoError(t, err)

	state := firstAttempt.Credentials[0].State
	require.NotEmpty(t, state)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, attempt := range []*sharing.Sharing{firstAttempt, secondAttempt} {
		attempt := attempt
		go func() {
			<-start
			errs <- attempt.SendAnswer(env.betty, state)
		}()
	}
	close(start)

	err1 := <-errs
	err2 := <-errs
	require.NoError(t, err1)
	require.NoError(t, err2)

	recipientSharing, err := sharing.FindSharing(env.betty, sharingID)
	require.NoError(t, err)
	require.True(t, recipientSharing.Active)
	require.Len(t, recipientSharing.Members, 2)
	require.Equal(t, sharing.MemberStatusReady, recipientSharing.Members[1].Status)
}

func TestDriveAutoAcceptAfterDiscoveryAuthorizeDoesNotConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupDriveAutoAcceptEnv(t, 200*time.Millisecond)

	sharingID, _, _ := createSharedDrive(
		t,
		DriveCreationMethodLegacy,
		env.ownerInstance,
		env.ownerAppToken,
		env.ownerURL,
		"Discovery auto-accept drive",
		"Discovery auto-accept test drive",
		[]RecipientInfo{{Name: "Recipient", Email: "recipient@example.com"}},
	)

	loginSharingRecipient(t, env.eRecipient)
	authorizeLink := prepareSharingAuthorizeLink(t, env.ownerInstance, "Recipient", sharingID, env.eOwner, env.recipientURL)

	recipientSharing := waitForSharingOnRecipient(t, env.recipientInstance, sharingID)
	FakeOwnerInstanceForSharing(t, env.recipientInstance, env.ownerURL, sharingID)
	require.False(t, recipientSharing.Active)

	assertNoAutoAcceptJobForSharing(t, env.recipientInstance, sharingID)
	openSharingAuthorize(t, env.eRecipient, authorizeLink, sharingID)
	waitForDriveSharingReadyOnOwner(t, env.eOwner, env.ownerAppToken, sharingID)
	waitForDriveSharingActiveOnRecipient(t, env.recipientInstance, sharingID)
}

func TestFileRootSharedDriveAuthorizeRedirect(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)
	rootFileID := createFile(t, eA, "", "SharedDriveRootFile.txt", env.acmeToken)
	sharingID, _ := createFileRootSharedDrive(
		t,
		env.acme,
		env.acmeToken,
		env.tsA.URL,
		rootFileID,
		"File-root authorize redirect",
		[]RecipientInfo{{Name: "Betty", Email: "betty@example.net"}},
	)

	loginSharingRecipient(t, eB)
	authorizeLink := prepareSharingAuthorizeLink(t, env.acme, "Betty", sharingID, eA, env.tsB.URL)
	FakeOwnerInstanceForSharing(t, env.betty, env.tsA.URL, sharingID)

	authorizeURL, err := url.Parse(authorizeLink)
	require.NoError(t, err)
	state := authorizeURL.Query().Get("state")
	require.NotEmpty(t, state)

	eB.GET(authorizeURL.Path).
		WithQuery("sharing_id", sharingID).
		WithQuery("state", state).
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().
		Status(http.StatusSeeOther).
		Header("Location").
		Contains("#/sharings/shareddrive/" + sharingID + "/file/" + rootFileID)
}

func TestSharedDriveAcceptance(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("RecipientSeesReadyStatus", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		obj := eB.GET("/sharings/"+env.firstSharingID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Value("data").Object().Value("attributes").Object()
		members := attrs.Value("members").Array()
		members.Length().Ge(2)
		members.Value(1).Object().Value("status").IsEqual("ready")
	})

	t.Run("OwnerSeesRecipientReadyStatus", func(t *testing.T) {
		eA, _, _ := env.createClients(t)

		obj := eA.GET("/sharings/"+env.firstSharingID).
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Value("data").Object().Value("attributes").Object()
		members := attrs.Value("members").Array()
		members.Length().Ge(2)
		members.Value(1).Object().Value("status").IsEqual("ready")
	})

	t.Run("AcceptNewDriveViaPOST", func(t *testing.T) {
		// Create a new drive that Dave hasn't accepted yet
		newSharingID, _, _ := createSharedDrive(
			t,
			DriveCreationMethodLegacy,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			"Drive for Dave POST acceptance",
			"Testing POST acceptance flow",
			nil,
		)

		// Dave accepts the sharing
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, newSharingID)

		// Verify Dave can now access the sharing
		_, _, eD := env.createClients(t)
		eD.GET("/sharings/"+newSharingID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)
	})
}

func TestSharedDriveDelegatedRecipientAddition(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	_, eBetty, eDave := env.createClients(t)

	t.Run("WriteRecipientCanInviteReadWrite", func(t *testing.T) {
		sharingID := createAcceptedSharedDriveForRecipient(
			t,
			env,
			RecipientInfo{Name: "Betty", Email: "betty@example.net", ReadOnly: false},
			env.betty,
			env.tsB.URL,
		)
		contact := createContact(t, env.betty, "Charlie", "charlie@example.net")

		eBetty.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", jsonAPIContentType).
			WithBytes(makeAddRecipientsPayload(t, sharingID, "recipients", contact.ID())).
			Expect().Status(http.StatusOK)

		added := findSharingMemberByEmail(t, env.acme, sharingID, "charlie@example.net")
		require.False(t, added.ReadOnly)
	})

	t.Run("WriteRecipientCanInviteReadOnly", func(t *testing.T) {
		sharingID := createAcceptedSharedDriveForRecipient(
			t,
			env,
			RecipientInfo{Name: "Betty", Email: "betty@example.net", ReadOnly: false},
			env.betty,
			env.tsB.URL,
		)
		contact := createContact(t, env.betty, "Rita", "rita@example.net")

		eBetty.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", jsonAPIContentType).
			WithBytes(makeAddRecipientsPayload(t, sharingID, "read_only_recipients", contact.ID())).
			Expect().Status(http.StatusOK)

		added := findSharingMemberByEmail(t, env.acme, sharingID, "rita@example.net")
		require.True(t, added.ReadOnly)
	})

	t.Run("AlreadyActiveRecipientIsIgnoredInMixedBatch", func(t *testing.T) {
		sharingID := createAcceptedSharedDriveForRecipient(
			t,
			env,
			RecipientInfo{Name: "Betty", Email: "betty@example.net", ReadOnly: false},
			env.betty,
			env.tsB.URL,
		)
		charlie := createContact(t, env.betty, "Charlie Mixed", "charlie-mixed@example.net")
		duplicate := createContact(t, env.betty, "Betty Duplicate", "betty@example.net")
		erin := createContact(t, env.betty, "Erin Mixed", "erin-mixed@example.net")

		eBetty.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", jsonAPIContentType).
			WithBytes(makeAddRecipientsPayload(t, sharingID, "recipients", charlie.ID(), duplicate.ID(), erin.ID())).
			Expect().Status(http.StatusOK)

		s, err := sharing.FindSharing(env.acme, sharingID)
		require.NoError(t, err)
		require.Len(t, s.Members, 4)
		require.Equal(t, sharing.MemberStatusReady, findSharingMemberByEmail(t, env.acme, sharingID, "betty@example.net").Status)
		findSharingMemberByEmail(t, env.acme, sharingID, "charlie-mixed@example.net")
		findSharingMemberByEmail(t, env.acme, sharingID, "erin-mixed@example.net")

		bettyCount := 0
		for _, member := range s.Members {
			if member.Email == "betty@example.net" {
				bettyCount++
			}
		}
		require.Equal(t, 1, bettyCount)
		require.ElementsMatch(t, []string{"Charlie Mixed", "Erin Mixed"}, sharingInvitationRecipientNames(t, env.betty, sharingID))
	})

	t.Run("CredentialsArePersistedBeforeCozyAutoAccept", func(t *testing.T) {
		sharingID := createAcceptedSharedDriveForRecipient(
			t,
			env,
			RecipientInfo{Name: "Betty Auto Accept", Email: "betty@example.net", ReadOnly: false},
			env.betty,
			env.tsB.URL,
		)
		eOwner, eBetty, _ := env.createClients(t)

		ownerSharing, err := sharing.FindSharing(env.acme, sharingID)
		require.NoError(t, err)
		require.NotEmpty(t, ownerSharing.Rules)
		require.NotEmpty(t, ownerSharing.Rules[0].Values)
		fileID := createFile(t, eOwner, ownerSharing.Rules[0].Values[0], "auto-accept.txt", env.acmeToken)

		render, _ := statik.NewDirRenderer("../../assets")
		autoAcceptResult := make(chan error, 1)
		var invitee *instance.Instance
		invitee, inviteeToken, inviteeServer := newDriveRecipientTestServer(
			t,
			testify(t, "delegated_auto_accept_invitee"),
			"invitee-auto-accept@example.net",
			"Invitee Auto Accept",
			render,
			map[string]func(*echo.Group){
				"/sharings": func(g *echo.Group) {
					g.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
						return func(c echo.Context) error {
							if err := next(c); err != nil {
								return err
							}
							if c.Request().Method != http.MethodPut ||
								c.QueryParam("shortcut") != "true" ||
								c.Param("sharing-id") != sharingID {
								return nil
							}

							recipientSharing, err := sharing.FindSharing(invitee, sharingID)
							if err == nil && (len(recipientSharing.Members) == 0 || len(recipientSharing.Credentials) == 0) {
								err = sharing.ErrInvalidSharing
							}
							if err == nil {
								recipientSharing.Members[0].Instance = env.tsA.URL
								err = couchdb.UpdateDoc(invitee, recipientSharing)
							}
							if err == nil {
								err = sharing.HandleAutoAccept(invitee, &sharing.AutoAcceptMsg{
									SharingID: sharingID,
									State:     recipientSharing.Credentials[0].State,
								})
							}
							select {
							case autoAcceptResult <- err:
							default:
							}
							return nil
						}
					})
					sharings.Routes(g)
				},
			},
		)

		inviteeContact := createContactWithCozy(
			t,
			env.betty,
			"Invitee Auto Accept",
			"invitee-auto-accept@example.net",
			inviteeServer.URL,
		)
		duplicate := createContactWithCozy(
			t,
			env.betty,
			"Betty Duplicate",
			"betty@example.net",
			env.tsB.URL,
		)

		eBetty.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", jsonAPIContentType).
			WithBytes(makeAddRecipientsPayload(t, sharingID, "recipients", inviteeContact.ID(), duplicate.ID())).
			Expect().Status(http.StatusOK)

		select {
		case err := <-autoAcceptResult:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "auto-accept did not run during the shortcut request")
		}

		require.Eventually(t, func() bool {
			s, err := sharing.FindSharing(env.acme, sharingID)
			if err != nil {
				return false
			}
			for _, member := range s.Members {
				if member.Email == "invitee-auto-accept@example.net" {
					return member.Status == sharing.MemberStatusReady
				}
			}
			return false
		}, 5*time.Second, 50*time.Millisecond, "owner should see the auto-accepted recipient as ready")

		require.Eventually(t, func() bool {
			s, err := sharing.FindSharing(invitee, sharingID)
			return err == nil && s.Active
		}, 5*time.Second, 50*time.Millisecond, "invitee sharing should be active")

		require.Eventually(t, func() bool {
			req, err := http.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				inviteeServer.URL+"/sharings/drives/"+sharingID+"/"+fileID,
				nil,
			)
			if err != nil {
				return false
			}
			req.Header.Set("Authorization", "Bearer "+inviteeToken)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			_ = res.Body.Close()
			return res.StatusCode == http.StatusOK
		}, 10*time.Second, 100*time.Millisecond, "auto-accepted recipient should access shared files")
	})

	t.Run("GroupMemberWithoutAddressIsSynchronizedWithoutInvitation", func(t *testing.T) {
		sharingID := createAcceptedSharedDriveForRecipient(
			t,
			env,
			RecipientInfo{Name: "Betty", Email: "betty@example.net", ReadOnly: false},
			env.betty,
			env.tsB.URL,
		)
		group := createGroupOnInstance(t, env.betty, "No address group")
		groupMember := contact.New()
		groupMember.M["fullname"] = "No Address"
		groupMember.M["relationships"] = map[string]interface{}{
			"groups": map[string]interface{}{
				"data": []interface{}{
					map[string]interface{}{
						"_id":   group.ID(),
						"_type": consts.Groups,
					},
				},
			},
		}
		require.NoError(t, couchdb.CreateDoc(env.betty, groupMember))

		eBetty.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", jsonAPIContentType).
			WithBytes(mustJSON(t, map[string]interface{}{
				"data": map[string]interface{}{
					"type": consts.Sharings,
					"id":   sharingID,
					"relationships": map[string]interface{}{
						"recipients": map[string]interface{}{
							"data": []map[string]interface{}{
								{"id": group.ID(), "type": consts.Groups},
							},
						},
					},
				},
			})).
			Expect().Status(http.StatusOK)

		for _, inst := range []*instance.Instance{env.acme, env.betty} {
			member := findSharingMemberByName(t, inst, sharingID, "No Address")
			require.Equal(t, sharing.MemberStatusMailNotSent, member.Status)
			require.True(t, member.OnlyInGroups)
			require.Empty(t, member.Email)
			require.Empty(t, member.Instance)
		}
		require.Empty(t, sharingInvitationRecipientNames(t, env.betty, sharingID))
	})

	t.Run("GroupColorIsPropagated", func(t *testing.T) {
		sharingID := createAcceptedSharedDriveForRecipient(
			t,
			env,
			RecipientInfo{Name: "Betty", Email: "betty@example.net", ReadOnly: false},
			env.betty,
			env.tsB.URL,
		)
		group := createGroupOnInstance(t, env.betty, "Marketing")
		group.M["color"] = "#3367D6"
		require.NoError(t, couchdb.UpdateDoc(env.betty, group))
		createContactInGroupWithCozy(
			t,
			env.betty,
			group,
			"Charlie Group",
			"charlie-group@example.net",
			"",
		)

		eBetty.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", jsonAPIContentType).
			WithBytes(mustJSON(t, map[string]interface{}{
				"data": map[string]interface{}{
					"type": consts.Sharings,
					"id":   sharingID,
					"relationships": map[string]interface{}{
						"recipients": map[string]interface{}{
							"data": []map[string]interface{}{
								{"id": group.ID(), "type": consts.Groups},
							},
						},
					},
				},
			})).
			Expect().Status(http.StatusOK)

		ownerSharing, err := sharing.FindSharing(env.acme, sharingID)
		require.NoError(t, err)
		require.NotEmpty(t, ownerSharing.Groups)
		require.Equal(t, "#3367D6", ownerSharing.Groups[len(ownerSharing.Groups)-1].Color)
	})

	t.Run("ReadOnlyRecipientCanInviteReadOnly", func(t *testing.T) {
		sharingID := createAcceptedSharedDriveForRecipient(
			t,
			env,
			RecipientInfo{Name: "Dave", Email: "dave@example.net", ReadOnly: true},
			env.dave,
			env.tsD.URL,
		)
		contact := createContact(t, env.dave, "Erin", "erin@example.net")

		eDave.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.daveToken).
			WithHeader("Content-Type", jsonAPIContentType).
			WithBytes(makeAddRecipientsPayload(t, sharingID, "read_only_recipients", contact.ID())).
			Expect().Status(http.StatusOK)

		added := findSharingMemberByEmail(t, env.acme, sharingID, "erin@example.net")
		require.True(t, added.ReadOnly)
	})

	t.Run("ReadOnlyRecipientCannotInviteReadWrite", func(t *testing.T) {
		sharingID := createAcceptedSharedDriveForRecipient(
			t,
			env,
			RecipientInfo{Name: "Dave", Email: "dave@example.net", ReadOnly: true},
			env.dave,
			env.tsD.URL,
		)
		contact := createContact(t, env.dave, "Mallory", "mallory@example.net")

		eDave.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.daveToken).
			WithHeader("Content-Type", jsonAPIContentType).
			WithBytes(makeAddRecipientsPayload(t, sharingID, "recipients", contact.ID())).
			Expect().Status(http.StatusForbidden)

		s, err := sharing.FindSharing(env.acme, sharingID)
		require.NoError(t, err)
		require.Len(t, s.Members, 2)
	})
}

func TestSharedDriveDelegatedRecipientRemoval(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eBetty, eDave := env.createClients(t)

	t.Run("WriteRecipientCanRemoveAnotherRecipient", func(t *testing.T) {
		sharingID, rootDirID, _ := createSharedDrive(
			t,
			DriveCreationMethodLegacy,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			"Delegated Recipient Removal Drive",
			"Drive for delegated recipient removal tests",
			nil,
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, sharingID)
		FakeOwnerInstanceForSharing(t, env.betty, env.acme.PageURL("", nil), sharingID)
		fileID := createFile(t, eA, rootDirID, "DelegatedRemoval.txt", env.acmeToken)

		eDave.GET("/sharings/drives/"+sharingID+"/"+fileID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(http.StatusOK)

		eBetty.DELETE("/sharings/"+sharingID+"/recipients/2").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(http.StatusNoContent)

		removed := findSharingMemberByEmail(t, env.acme, sharingID, "dave@example.net")
		require.Equal(t, sharing.MemberStatusRevoked, removed.Status)

		eDave.GET("/sharings/drives/"+sharingID+"/"+fileID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(http.StatusForbidden)
	})

	t.Run("ReadOnlyRecipientCannotRemoveAnotherRecipient", func(t *testing.T) {
		sharingID, _, _ := createSharedDrive(
			t,
			DriveCreationMethodLegacy,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			"Delegated Recipient Removal Read Only Drive",
			"Drive for read-only delegated recipient removal tests",
			nil,
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, sharingID)
		FakeOwnerInstanceForSharing(t, env.dave, env.acme.PageURL("", nil), sharingID)

		eDave.DELETE("/sharings/"+sharingID+"/recipients/1").
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(http.StatusForbidden)

		betty := findSharingMemberByEmail(t, env.acme, sharingID, "betty@example.net")
		require.Equal(t, sharing.MemberStatusReady, betty.Status)
	})
}

func TestSharedDriveDelegatedPendingRecipientManagement(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	_, eBetty, _ := env.createClients(t)

	sharingID, _, _ := createSharedDrive(
		t,
		DriveCreationMethodLegacy,
		env.acme,
		env.acmeToken,
		env.tsA.URL,
		"Delegated Pending Recipient Management Drive",
		"Drive for delegated pending recipient management tests",
		[]RecipientInfo{
			{Name: "Betty", Email: "betty@example.net", ReadOnly: false},
			{Name: "Charlie", Email: "charlie@example.net", ReadOnly: false},
		},
	)
	acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

	charlieIndex := findSharingMemberIndexByEmail(t, env.acme, sharingID, "charlie@example.net")
	requireOwnerMemberState(t, env.acme, sharingID, "charlie@example.net", sharing.MemberStatusPendingInvitation, false)

	eBetty.POST(fmt.Sprintf("/sharings/%s/recipients/%d/readonly", sharingID, charlieIndex)).
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(http.StatusNoContent)
	requireOwnerMemberState(t, env.acme, sharingID, "charlie@example.net", sharing.MemberStatusPendingInvitation, true)

	eBetty.DELETE(fmt.Sprintf("/sharings/%s/recipients/%d/readonly", sharingID, charlieIndex)).
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(http.StatusNoContent)
	requireOwnerMemberState(t, env.acme, sharingID, "charlie@example.net", sharing.MemberStatusPendingInvitation, false)

	eBetty.DELETE(fmt.Sprintf("/sharings/%s/recipients/%d", sharingID, charlieIndex)).
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(http.StatusNoContent)
	requireOwnerMemberState(t, env.acme, sharingID, "charlie@example.net", sharing.MemberStatusRevoked, false)
}

func TestSharedDriveAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("HEAD", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		t.Run("NonExistentFile", func(t *testing.T) {
			eB.HEAD("/sharings/drives/"+env.firstSharingID+"/nonexistent").
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(404)
		})

		t.Run("Directory", func(t *testing.T) {
			eB.HEAD("/sharings/drives/"+env.firstSharingID+"/"+env.firstRootDirID).
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(200)
		})

		t.Run("Subdirectory", func(t *testing.T) {
			eB.HEAD("/sharings/drives/"+env.firstSharingID+"/"+env.meetingsDirID).
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(200)
		})

		t.Run("File", func(t *testing.T) {
			eB.HEAD("/sharings/drives/"+env.firstSharingID+"/"+env.checklistID).
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(200)
		})

		t.Run("WithoutAuth", func(t *testing.T) {
			eB.HEAD("/sharings/drives/" + env.firstSharingID + "/" + env.checklistID).
				Expect().Status(401)
		})

		t.Run("WrongSharingID", func(t *testing.T) {
			eB.HEAD("/sharings/drives/wrong-id/"+env.checklistID).
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(404)
		})
	})

	t.Run("GET", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		t.Run("NonExistentFile", func(t *testing.T) {
			eB.GET("/sharings/drives/"+env.firstSharingID+"/nonexistent").
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(404)
		})

		t.Run("Directory", func(t *testing.T) {
			obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/"+env.meetingsDirID).
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			data.Value("type").String().IsEqual("io.cozy.files")
			data.Value("id").String().IsEqual(env.meetingsDirID)
			attrs := data.Value("attributes").Object()
			attrs.Value("type").String().IsEqual("directory")
			attrs.Value("name").String().IsEqual("Meetings")
			attrs.Value("driveId").String().IsEqual(env.firstSharingID)
		})

		t.Run("File", func(t *testing.T) {
			obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/"+env.checklistID).
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			data.Value("type").String().IsEqual("io.cozy.files")
			data.Value("id").String().IsEqual(env.checklistID)
			attrs := data.Value("attributes").Object()
			attrs.Value("type").String().IsEqual("file")
			attrs.Value("name").String().IsEqual("Checklist.txt")
			attrs.Value("mime").String().IsEqual("text/plain")
			attrs.Value("size").String().IsEqual("3")
		})

		t.Run("WithoutAuth", func(t *testing.T) {
			eB.GET("/sharings/drives/" + env.firstSharingID + "/" + env.meetingsDirID).
				Expect().Status(401)
		})

		t.Run("WrongSharingID", func(t *testing.T) {
			eB.GET("/sharings/drives/wrong-id/"+env.meetingsDirID).
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(404)
		})
	})
}

func TestSharedDriveChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("GetChangesFeed", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/_changes").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/json"}).
			Object()

		// Should have results array
		results := obj.Value("results").Array()
		results.NotEmpty()

		// Should have last_seq
		obj.Value("last_seq").String().NotEmpty()
	})

	t.Run("ChangesIncludeSharedFiles", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/_changes").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/json"}).
			Object()

		results := obj.Value("results").Array()

		// Find the checklist file in the results
		found := false
		for i := 0; i < int(results.Length().Raw()); i++ {
			change := results.Value(i).Object()
			if change.Value("id").String().Raw() == env.checklistID {
				found = true
				// Verify driveId is set
				change.Path("$.doc.driveId").String().IsEqual(env.firstSharingID)
				break
			}
		}
		require.True(t, found, "Checklist file should be in changes feed")
	})

	t.Run("ChangesExcludeUnsharedFiles", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/_changes").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/json"}).
			Object()

		results := obj.Value("results").Array()

		// The outsideOfShareID should either not be present or be marked as deleted
		for i := 0; i < int(results.Length().Raw()); i++ {
			change := results.Value(i).Object()
			if change.Value("id").String().Raw() == env.outsideOfShareID {
				// If present, should be marked as deleted
				change.Value("deleted").Boolean().IsTrue()
			}
		}
	})

	t.Run("ChangesWithoutAuth", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.GET("/sharings/drives/" + env.firstSharingID + "/_changes").
			Expect().Status(401)
	})

	t.Run("ChangesWrongSharingID", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.GET("/sharings/drives/wrong-id/_changes").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(404)
	})
}

func TestFileRootSharedDriveChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)

	rootFileID := createFile(t, eA, "", "FileRootChanges.txt", env.acmeToken)
	unrelatedFileID := createFile(t, eA, "", "OutsideInitialChanges.txt", env.acmeToken)
	sharingID, _ := createFileRootSharedDrive(
		t,
		env.acme,
		env.acmeToken,
		env.tsA.URL,
		rootFileID,
		"File-root changes drive",
		[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
	)
	acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

	initial := eB.GET("/sharings/drives/"+sharingID+"/_changes").
		WithQuery("include_docs", true).
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/json"}).
		Object()

	lastSeq := initial.Value("last_seq").String().NotEmpty().Raw()
	results := initial.Value("results").Array()
	foundRoot := false
	foundUnrelated := false
	for i := 0; i < int(results.Length().Raw()); i++ {
		change := results.Value(i).Object()
		switch change.Value("id").String().Raw() {
		case rootFileID:
			foundRoot = true
			change.Path("$.doc.driveId").String().IsEqual(sharingID)
		case unrelatedFileID:
			foundUnrelated = true
		}
	}
	require.True(t, foundRoot, "root file should be present in file-root changes feed")
	require.False(t, foundUnrelated, "unrelated files should not be present in file-root changes feed")

	eA.PATCH("/sharings/drives/"+sharingID+"/"+rootFileID).
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(`{
			"data": {
				"type": "io.cozy.files",
				"id": "` + rootFileID + `",
				"attributes": {
					"name": "FileRootChangesRenamed.txt"
				}
			}
		}`)).
		Expect().Status(200)

	newUnrelatedFileID := createFile(t, eA, "", "OutsideSinceChanges.txt", env.acmeToken)

	since := eB.GET("/sharings/drives/"+sharingID+"/_changes").
		WithQuery("since", lastSeq).
		WithQuery("include_docs", true).
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/json"}).
		Object()

	sinceResults := since.Value("results").Array()
	foundRootUpdate := false
	foundNewUnrelated := false
	for i := 0; i < int(sinceResults.Length().Raw()); i++ {
		change := sinceResults.Value(i).Object()
		switch change.Value("id").String().Raw() {
		case rootFileID:
			foundRootUpdate = true
			change.Path("$.doc.driveId").String().IsEqual(sharingID)
			change.Path("$.doc.name").String().IsEqual("FileRootChangesRenamed.txt")
		case newUnrelatedFileID:
			foundNewUnrelated = true
		}
	}
	require.True(t, foundRootUpdate, "root file update should be present in file-root changes feed")
	require.False(t, foundNewUnrelated, "unrelated changes should be filtered out from file-root changes feed")
}

func TestSharedDriveCreation(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("ListSharedDrives", func(t *testing.T) {
		eA, _, _ := env.createClients(t)

		obj := eA.GET("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().Ge(1) // At least one drive from setup

		// Verify first drive has expected structure
		sharingObj := data.Value(0).Object()
		sharingObj.Value("type").IsEqual("io.cozy.sharings")
		sharingObj.Value("id").String().NotEmpty()

		attrs := sharingObj.Value("attributes").Object()
		attrs.Value("app_slug").IsEqual("drive")
		attrs.Value("owner").IsEqual(true)
		attrs.Value("drive").IsEqual(true)
		attrs.Value("drive_root_type").IsEqual("directory")

		members := attrs.Value("members").Array()
		members.Length().Ge(2) // At least owner + Betty

		owner := members.Value(0).Object()
		owner.Value("status").IsEqual("owner")
	})

	t.Run("CreateAdditionalDrive", func(t *testing.T) {
		// Create a new shared drive
		sharingID, dirID, _ := createSharedDrive(
			t,
			DriveCreationMethodLegacy,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			"Additional Test Drive",
			"Drive created in TestSharedDriveCreation",
			nil,
		)

		require.NotEmpty(t, sharingID, "Sharing ID should not be empty")
		require.NotEmpty(t, dirID, "Directory ID should not be empty")

		// Verify the drive appears in the list
		eA, _, _ := env.createClients(t)
		obj := eA.GET("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().Ge(2) // Original + new drive
	})

	t.Run("CreateDriveFromFolder", func(t *testing.T) {
		sharingID, dirID, _ := createSharedDrive(
			t,
			DriveCreationMethodFromFolder,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			"Drive From Folder Test",
			"Drive created from existing folder",
			nil,
		)

		require.NotEmpty(t, sharingID, "Sharing ID should not be empty")
		require.NotEmpty(t, dirID, "Directory ID should not be empty")
	})

	t.Run("CreateDriveFromFile", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		fileID := createFile(t, eA, "", "SharedDriveFile.txt", env.acmeToken)

		obj := eA.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "Drive created from file",
						"file_id": "%s"
					}
				}
			}`, consts.Sharings, fileID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		sharingID := obj.Path("$.data.id").String().NotEmpty().Raw()
		attrs := obj.Path("$.data.attributes").Object()
		attrs.Value("drive").Boolean().IsTrue()
		attrs.Value("drive_root_type").String().IsEqual("file")
		attrs.Value("description").String().IsEqual("Drive created from file")
		rule := attrs.Value("rules").Array().Value(0).Object()
		rule.Value("title").String().IsEqual("SharedDriveFile.txt")
		rule.Value("mime").String().IsEqual("text/plain")
		rule.Value("values").Array().Value(0).String().IsEqual(fileID)

		sharedFile, err := env.acme.VFS().FileByID(fileID)
		require.NoError(t, err)
		require.Contains(t, sharedFile.ReferencedBy, couchdb.DocReference{
			ID:   sharingID,
			Type: consts.Sharings,
		})

		eA.GET("/sharings/"+sharingID).
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.drive_root_type").String().IsEqual("file")

		listObj := eA.GET("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		found := false
		for _, item := range listObj.Path("$.data").Array().Iter() {
			drive := item.Object()
			if drive.Value("id").String().Raw() != sharingID {
				continue
			}
			drive.Path("$.attributes.drive_root_type").String().IsEqual("file")
			found = true
		}
		require.True(t, found, "file-root shared drive should be listed")
	})

	t.Run("CreateDriveFromFolderViaFileID", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		dirID := createRootDirectory(t, eA, "SharedDriveFolderViaFileID", env.acmeToken)

		obj := eA.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"file_id": "%s"
					}
				}
			}`, consts.Sharings, dirID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.drive_root_type").String().IsEqual("directory")
		obj.Path("$.data.attributes.rules[0].values[0]").String().IsEqual(dirID)
	})

	t.Run("CreateDriveFromFileViaFolderID", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		fileID := createFile(t, eA, "", "SharedDriveFileViaFolderID.txt", env.acmeToken)

		obj := eA.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, fileID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.drive_root_type").String().IsEqual("file")
		obj.Path("$.data.attributes.rules[0].values[0]").String().IsEqual(fileID)
	})

	t.Run("LegacyDriveCreationInfersFileRootType", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		fileID := createFile(t, eA, "", "LegacyDriveFile.txt", env.acmeToken)

		obj := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "Legacy file-root shared drive",
						"drive": true,
						"rules": [{
							"title": "LegacyDriveFile.txt",
							"doctype": "%s",
							"values": ["%s"]
						}]
					}
				}
			}`, consts.Sharings, consts.Files, fileID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.drive_root_type").String().IsEqual("file")
		obj.Path("$.data.attributes.rules[0].mime").String().IsEqual("text/plain")
	})

	t.Run("LegacyDriveCreationKeepsExplicitFileRootTypeAndAddsMime", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		fileID := createFile(t, eA, "", "LegacyExplicitDriveFile.txt", env.acmeToken)

		obj := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "Legacy explicit file-root shared drive",
						"drive": true,
						"drive_root_type": "file",
						"rules": [{
							"title": "LegacyExplicitDriveFile.txt",
							"doctype": "%s",
							"values": ["%s"]
						}]
					}
				}
			}`, consts.Sharings, consts.Files, fileID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.drive_root_type").String().IsEqual("file")
		obj.Path("$.data.attributes.rules[0].mime").String().IsEqual("text/plain")
	})

	t.Run("LegacyDriveCreationOverridesClientFileRootMime", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		fileID := createFileWithMime(t, eA, "", "LegacyBinaryDriveFile.bin", env.acmeToken, "application/octet-stream")

		obj := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "Legacy binary file-root shared drive",
						"drive": true,
						"drive_root_type": "file",
						"rules": [{
							"title": "LegacyBinaryDriveFile.bin",
							"doctype": "%s",
							"mime": "text/plain",
							"values": ["%s"]
						}]
					}
				}
			}`, consts.Sharings, consts.Files, fileID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.drive_root_type").String().IsEqual("file")
		obj.Path("$.data.attributes.rules[0].mime").String().IsEqual("application/octet-stream")
	})

	t.Run("LegacyDriveCreationClearsDirectoryRuleMime", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		dirID := createRootDirectory(t, eA, "LegacyDirectoryDriveWithMime", env.acmeToken)

		obj := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "Legacy directory-root shared drive",
						"drive": true,
						"drive_root_type": "directory",
						"rules": [{
							"title": "LegacyDirectoryDriveWithMime",
							"doctype": "%s",
							"mime": "evil/foo",
							"values": ["%s"]
						}]
					}
				}
			}`, consts.Sharings, consts.Files, dirID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.drive_root_type").String().IsEqual("directory")
		obj.Path("$.data.attributes.rules[0]").Object().NotContainsKey("mime")
	})

	t.Run("LegacyDriveCreationCanonicalizesExplicitRootType", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		fileID := createFile(t, eA, "", "LegacyCanonicalDriveFile.txt", env.acmeToken)

		obj := eA.POST("/sharings/").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "Legacy canonical file-root shared drive",
						"drive": true,
						"drive_root_type": "directory",
						"rules": [{
							"title": "LegacyCanonicalDriveFile.txt",
							"doctype": "%s",
							"values": ["%s"]
						}]
					}
				}
			}`, consts.Sharings, consts.Files, fileID))).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.drive_root_type").String().IsEqual("file")
		obj.Path("$.data.attributes.rules[0].mime").String().IsEqual("text/plain")
	})
}

func TestOrgDriveFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	const orgSlug = "org-slug"

	env := setupSharedDrivesEnvWithOwnerOptions(t, &lifecycle.Options{
		Domain:     orgSlug + ".example.net",
		OrgID:      orgSlug,
		Email:      "owner@example.net",
		PublicName: "Owner",
	})
	eOwner, _, _ := env.createClients(t)
	folderID := createRootDirectory(t, eOwner, "OrgDriveRoot", env.acmeToken)
	recipientContact := createContact(t, env.acme, "Betty OrgDrive", "betty-orgdrive@example.net")

	obj := eOwner.POST("/sharings/drives").
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(fmt.Sprintf(`{
			"data": {
				"type": "%s",
				"attributes": {
					"description": "Organization drive",
					"folder_id": "%s"
				},
				"relationships": {
					"recipients": {
						"data": [{"id": "%s", "type": "%s"}]
					}
				}
			}
		}`, consts.Sharings, folderID, recipientContact.ID(), consts.Contacts))).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	sharingID := obj.Path("$.data.id").String().NotEmpty().Raw()
	attrs := obj.Path("$.data.attributes").Object()
	attrs.Value("drive").Boolean().IsTrue()
	attrs.Value("org_drive").Boolean().IsTrue()

	getOrgDriveFlag := func(baseURL, token string) (bool, error) {
		u, err := url.Parse(baseURL)
		if err != nil {
			return false, err
		}
		res, err := request.Req(&request.Options{
			Method: http.MethodGet,
			Scheme: u.Scheme,
			Domain: u.Host,
			Path:   "/sharings/" + sharingID,
			Headers: request.Headers{
				echo.HeaderAuthorization: "Bearer " + token,
				echo.HeaderAccept:        "application/vnd.api+json",
			},
		})
		if err != nil {
			return false, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return false, fmt.Errorf("unexpected status: %d", res.StatusCode)
		}

		var payload struct {
			Data struct {
				Attributes struct {
					OrgDrive bool `json:"org_drive"`
				} `json:"attributes"`
			} `json:"data"`
		}
		if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
			return false, err
		}
		return payload.Data.Attributes.OrgDrive, nil
	}

	ownerOrgDrive, err := getOrgDriveFlag(env.tsA.URL, env.acmeToken)
	require.NoError(t, err)
	require.True(t, ownerOrgDrive)

	acceptSharedDrive(t, env.acme, env.betty, "Betty OrgDrive", env.tsA.URL, env.tsB.URL, sharingID)

	require.Eventually(t, func() bool {
		recipientOrgDrive, err := getOrgDriveFlag(env.tsB.URL, env.bettyToken)
		return err == nil && recipientOrgDrive
	}, 10*time.Second, 200*time.Millisecond, "recipient sharing should preserve org_drive")

	eOwner.DELETE("/sharings/"+sharingID+"/recipients").
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		Expect().Status(http.StatusNoContent)

	var revoked sharing.Sharing
	require.Eventually(t, func() bool {
		if err := couchdb.GetDoc(env.acme, consts.Sharings, sharingID, &revoked); err != nil {
			return false
		}
		return revoked.OrgDrive && !revoked.Active
	}, 5*time.Second, 100*time.Millisecond, "owner org-drive sharing should be kept inactive after revocation")
	require.Len(t, revoked.Members, 2)
	assert.Equal(t, sharing.MemberStatusRevoked, revoked.Members[1].Status)
	requireNoDirSharingReference(t, env.acme, folderID, sharingID)
}

func TestSharedDriveTrashAttribution(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	_, eB, _ := env.createClients(t)

	publicName, err := env.betty.SettingsPublicName()
	require.NoError(t, err)

	obj := eB.DELETE("/sharings/drives/"+env.firstSharingID+"/"+env.checklistID).
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	obj.Path("$.data.attributes.trashed").Boolean().True()
	fcm := obj.Path("$.data.attributes.cozyMetadata").Object()
	trashedAt := fcm.Value("trashedAt").String().NotEmpty().Raw()
	trashedBy := fcm.Value("trashedBy").Object()
	trashedByKind := trashedBy.Value("kind").String().NotEmpty().Raw()
	trashedByDisplayName := trashedBy.Value("displayName").String().NotEmpty().Raw()
	trashedByDomain := trashedBy.Value("domain").String().NotEmpty().Raw()
	require.Equal(t, vfs.TrashedByKindMember, trashedByKind)
	require.Equal(t, publicName, trashedByDisplayName)

	type filePayload struct {
		Data struct {
			Attributes struct {
				Trashed      bool `json:"trashed"`
				CozyMetadata struct {
					TrashedAt string `json:"trashedAt"`
					TrashedBy struct {
						Kind        string `json:"kind"`
						DisplayName string `json:"displayName"`
						Domain      string `json:"domain"`
					} `json:"trashedBy"`
				} `json:"cozyMetadata"`
			} `json:"attributes"`
		} `json:"data"`
	}

	getFilePayload := func(baseURL, token, requestPath string) (*filePayload, error) {
		u, err := url.Parse(baseURL)
		if err != nil {
			return nil, err
		}
		res, err := request.Req(&request.Options{
			Method: http.MethodGet,
			Scheme: u.Scheme,
			Domain: u.Host,
			Path:   requestPath,
			Headers: request.Headers{
				echo.HeaderAuthorization: "Bearer " + token,
				echo.HeaderAccept:        "application/vnd.api+json",
			},
		})
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status: %d", res.StatusCode)
		}
		var payload filePayload
		if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
			return nil, err
		}
		return &payload, nil
	}

	require.Eventually(t, func() bool {
		payload, err := getFilePayload(env.tsA.URL, env.acmeToken, "/files/"+env.checklistID)
		if err != nil {
			return false
		}
		return payload.Data.Attributes.Trashed &&
			payload.Data.Attributes.CozyMetadata.TrashedAt == trashedAt &&
			payload.Data.Attributes.CozyMetadata.TrashedBy.Kind == trashedByKind &&
			payload.Data.Attributes.CozyMetadata.TrashedBy.DisplayName == trashedByDisplayName &&
			payload.Data.Attributes.CozyMetadata.TrashedBy.Domain == trashedByDomain
	}, 10*time.Second, 200*time.Millisecond, "owner should receive recipient trash attribution")
}

func TestFileRootSharedDriveReadRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("HeadGetAndDownloadRootFile", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		rootFileID := createFile(t, eA, "", "SharedDriveRootFile.txt", env.acmeToken)
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			"File-root shared drive",
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)
		unrelatedFileID := createFile(t, eA, "", "OutsideFile.txt", env.acmeToken)

		eA.HEAD("/sharings/drives/"+sharingID+"/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(200)

		obj := eB.GET("/sharings/drives/"+sharingID+"/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.id").String().IsEqual(rootFileID)
		obj.Path("$.data.type").String().IsEqual(consts.Files)
		obj.Path("$.data.attributes.name").String().IsEqual("SharedDriveRootFile.txt")

		res := eB.GET("/sharings/drives/"+sharingID+"/download/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
		res.Header("Content-Disposition").IsEqual(`inline; filename="SharedDriveRootFile.txt"`)
		res.Body().IsEqual("foo")

		related := eB.POST("/sharings/drives/"+sharingID+"/downloads").
			WithQuery("Id", rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.links.related").String().NotEmpty().Raw()

		eB.GET(related).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			Body().IsEqual("foo")

		eB.GET("/sharings/drives/"+sharingID+"/"+unrelatedFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(403)

		eB.GET("/sharings/drives/"+sharingID+"/download/"+unrelatedFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(403)
	})

	t.Run("OpenNoteRootFile", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		noteObj := eA.POST("/notes").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.notes.documents",
					"attributes": {
						"title": "Root Note",
						"schema": {
							"nodes": [
								["doc", { "content": "block+" }],
								["paragraph", { "content": "inline*", "group": "block" }],
								["text", { "group": "inline" }]
							],
							"marks": [],
							"topNode": "doc"
						}
					}
				}
			}`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		noteID := noteObj.Value("data").Object().Value("id").String().Raw()
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			noteID,
			"Note-backed drive",
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

		obj := eB.GET("/sharings/drives/"+sharingID+"/notes/"+noteID+"/open").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.type").String().IsEqual(consts.NotesURL)
		obj.Path("$.data.id").String().IsEqual(noteID)
		obj.Path("$.data.attributes.note_id").String().IsEqual(noteID)
		obj.Path("$.data.attributes.sharecode").String().NotEmpty()
	})
}

func TestFileRootSharedDriveMutationRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("MutateRootFile", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		rootFileID := createFile(t, eA, "", "MutableRoot.txt", env.acmeToken)
		outsideDirID := createRootDirectory(t, eA, "OutsidePatchTarget", env.acmeToken)
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			"Mutable file-root shared drive",
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

		eB.PUT("/sharings/drives/"+sharingID+"/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte("bar")).
			Expect().Status(200)

		eB.GET("/sharings/drives/"+sharingID+"/download/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			Body().IsEqual("bar")

		versioned := eB.POST("/sharings/drives/"+sharingID+"/"+rootFileID+"/versions").
			WithQuery("Tags", "checkpoint").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
					"data": {
						"type": "io.cozy.files.metadata",
						"attributes": {
							"label": "root-version"
						}
					}
				}`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		versionedAttrs := versioned.Path("$.data.attributes").Object()
		versionedTags := versionedAttrs.Value("tags").Array()
		versionedTags.Length().Equal(1)
		versionedTags.First().String().IsEqual("checkpoint")
		versionedAttrs.Value("metadata").Object().ValueEqual("label", "root-version")

		eB.GET("/sharings/drives/"+sharingID+"/download/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			Body().IsEqual("bar")

		patched := eB.PATCH("/sharings/drives/"+sharingID+"/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.files",
					"id": "` + rootFileID + `",
					"attributes": {
						"name": "RenamedRoot.txt",
						"cozyMetadata": {
							"favorite": true
						}
					}
				}
			}`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		patched.Path("$.data.attributes.name").String().IsEqual("RenamedRoot.txt")
		patched.Path("$.data.attributes.cozyMetadata.favorite").Boolean().True()
		require.Eventually(t, func() bool {
			ownerSharing, err := sharing.FindSharing(env.acme, sharingID)
			if err != nil || ownerSharing.Description != "RenamedRoot.txt" ||
				len(ownerSharing.Rules) == 0 || ownerSharing.Rules[0].Title != "RenamedRoot.txt" {
				return false
			}
			recipientSharing, err := sharing.FindSharing(env.betty, sharingID)
			if err != nil {
				return false
			}
			return recipientSharing.Description == "RenamedRoot.txt" &&
				len(recipientSharing.Rules) > 0 && recipientSharing.Rules[0].Title == "RenamedRoot.txt"
		}, 5*time.Second, 50*time.Millisecond)

		eB.PATCH("/sharings/drives/"+sharingID+"/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.files",
					"id": "` + rootFileID + `",
					"attributes": {
						"dir_id": "` + outsideDirID + `"
					}
				}
			}`)).
			Expect().Status(422)

		eB.DELETE("/sharings/drives/"+sharingID+"/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.trashed").Boolean().True()

		require.Eventually(t, func() bool {
			s, err := sharing.FindSharing(env.betty, sharingID)
			return err == nil && !s.Active
		}, 5*time.Second, 50*time.Millisecond)

		eB.POST("/sharings/drives/"+sharingID+"/trash/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(403)
	})

	t.Run("OwnerCanRenameFileRootViaFilesPatch", func(t *testing.T) {
		eA, _, _ := env.createClients(t)

		rootFileID := createFile(t, eA, "", "OwnerRenamedRoot.txt", env.acmeToken)
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			"Owner file-root shared drive",
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

		newDriveName := testify(t, "Owner renamed file-root via files patch")

		renamed := eA.PATCH("/files/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.files",
					"id": "` + rootFileID + `",
					"attributes": {
						"name": "` + newDriveName + `"
					}
				}
			}`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()
		renamed.Path("$.data.attributes.name").String().IsEqual(newDriveName)

		require.Eventually(t, func() bool {
			s, err := sharing.FindSharing(env.betty, sharingID)
			if err != nil {
				return false
			}
			return s.Description == newDriveName &&
				len(s.Rules) > 0 && s.Rules[0].Title == newDriveName
		}, 5*time.Second, 50*time.Millisecond)
	})

	t.Run("DeletingRootFileRevokesRecipient", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		rootFileID := createFile(t, eA, "", "DestroyableRoot.txt", env.acmeToken)
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			"Destroyable file-root shared drive",
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

		eB.DELETE("/sharings/drives/"+sharingID+"/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)

		require.Eventually(t, func() bool {
			s, err := sharing.FindSharing(env.betty, sharingID)
			return err == nil && !s.Active
		}, 5*time.Second, 50*time.Millisecond)

		eB.DELETE("/sharings/drives/"+sharingID+"/trash/"+rootFileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(403)
	})

	t.Run("RejectDirectoryOnlyRoutes", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		rootFileID := createFile(t, eA, "", "UnsupportedRoutesRoot.txt", env.acmeToken)
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			"Unsupported routes file-root shared drive",
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

		eB.GET("/sharings/drives/"+sharingID+"/metadata").
			WithQuery("Path", "/UnsupportedRoutesRoot.txt").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.type").String().IsEqual("file")

		eB.GET("/sharings/drives/"+sharingID+"/"+rootFileID+"/size").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(422)

		eB.POST("/sharings/drives/"+sharingID+"/"+rootFileID).
			WithQuery("Name", "Child.txt").
			WithQuery("Type", "file").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte("child")).
			Expect().Status(422)

		eB.POST("/sharings/drives/"+sharingID+"/upload/metadata").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.files.metadata",
					"attributes": {
						"device-id": "123456789"
					}
				}
			}`)).
			Expect().Status(422)

		eB.POST("/sharings/drives/"+sharingID+"/archive").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(422)

		eB.POST("/sharings/drives/"+sharingID+"/"+rootFileID+"/copy").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(422)

		eB.POST("/sharings/drives/"+sharingID+"/notes").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.notes.documents",
					"attributes": {
						"title": "Should Fail"
					}
				}
			}`)).
			Expect().Status(422)
	})
}

func TestSharedDriveDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("DirectDownload", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		res := eB.GET("/sharings/drives/"+env.firstSharingID+"/download/"+env.checklistID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)

		res.Header("Content-Disposition").HasPrefix("inline")
		res.Header("Content-Disposition").Contains("Checklist.txt")
		res.Header("Content-Type").IsEqual("text/plain")
		res.Header("Etag").NotEmpty()
		res.Header("Content-Length").IsEqual("3")
		res.Body().IsEqual("foo")
	})

	t.Run("TwoStepDownload", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		// Create download link
		related := eB.POST("/sharings/drives/"+env.firstSharingID+"/downloads").
			WithQuery("Id", env.checklistID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.links.related").String().NotEmpty().Raw()

		// Download inline
		res := eB.GET(related).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
		res.Header("Content-Disposition").IsEqual(`inline; filename="Checklist.txt"`)

		// Download as attachment
		res = eB.GET(related).
			WithQuery("Dl", "1").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
		res.Header("Content-Disposition").IsEqual(`attachment; filename="Checklist.txt"`)
	})

	t.Run("DownloadNonExistent", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.GET("/sharings/drives/"+env.firstSharingID+"/download/nonexistent").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(404)
	})

	t.Run("DownloadWithoutAuth", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.GET("/sharings/drives/" + env.firstSharingID + "/download/" + env.checklistID).
			Expect().Status(401)
	})
}

func TestSharedDriveShortcut(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)

	targetURL := "https://photos.example.net/#/photos/shortcut-target"
	shortcutID := createShortcut(t, eA, env.meetingsDirID, "Open photo.url", env.acmeToken, targetURL)

	_, err := env.betty.VFS().FileByID(shortcutID)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err), "shortcut should only exist on the owner instance")

	t.Run("JSON", func(t *testing.T) {
		obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/shortcuts/"+shortcutID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Accept", "application/json").
			Expect().Status(http.StatusOK).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.Value("type").String().IsEqual(consts.FilesShortcuts)
		data.Value("id").String().IsEqual(shortcutID)

		attrs := data.Value("attributes").Object()
		attrs.Value("name").String().IsEqual("Open photo.url")
		attrs.Value("dir_id").String().IsEqual(env.meetingsDirID)
		attrs.Value("url").String().IsEqual(targetURL)
	})

	t.Run("Redirect", func(t *testing.T) {
		eB.GET("/sharings/drives/"+env.firstSharingID+"/shortcuts/"+shortcutID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Accept", "text/html").
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(http.StatusSeeOther).
			Header("Location").IsEqual(targetURL)
	})

	t.Run("WithoutAuth", func(t *testing.T) {
		eB.GET("/sharings/drives/"+env.firstSharingID+"/shortcuts/"+shortcutID).
			WithHeader("Accept", "application/json").
			Expect().Status(http.StatusUnauthorized)
	})
}

func TestSharedDriveArchiveDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("ArchiveMultipleFiles", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Create a second file in the meetings directory (owned by ACME, shared to Betty)
		file2ID := createFile(t, eA, env.meetingsDirID, "Notes.txt", env.acmeToken)

		// Betty (recipient) creates an archive link for the two files
		body := fmt.Sprintf(`{"data":{"attributes":{"ids":[%q,%q]}}}`, env.checklistID, file2ID)
		related := eB.POST("/sharings/drives/"+env.firstSharingID+"/archive").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(body)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.links.related").String().NotEmpty().Raw()

		// The returned link must point to the shared drive archive endpoint, not /files/archive
		require.Contains(t, related, "/sharings/drives/"+env.firstSharingID+"/archive/")

		// Download the zip — Betty's token is enough (needsAuth=false for the GET)
		resp := eB.GET(related).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
		resp.Header("Content-Disposition").Contains(`attachment; filename="archive.zip"`)

		// Verify zip contains exactly the two requested files (archive/<filename>)
		zipBytes := []byte(resp.Body().Raw())
		z, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
		require.NoError(t, err)
		var names []string
		for _, f := range z.File {
			names = append(names, f.Name)
		}
		require.Contains(t, names, "archive/Checklist.txt")
		require.Contains(t, names, "archive/Notes.txt")
	})

	t.Run("ArchiveFolder", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		// Betty (recipient) creates an archive link for the whole meetings folder
		body := fmt.Sprintf(`{"data":{"attributes":{"ids":[%q]}}}`, env.meetingsDirID)
		related := eB.POST("/sharings/drives/"+env.firstSharingID+"/archive").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(body)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.links.related").String().NotEmpty().Raw()

		// The returned link must point to the shared drive archive endpoint
		require.Contains(t, related, "/sharings/drives/"+env.firstSharingID+"/archive/")

		// Download the zip — the folder and its contents are zipped on-the-fly
		resp := eB.GET(related).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
		resp.Header("Content-Disposition").Contains(`attachment; filename="archive.zip"`)

		// Verify zip contains the folder entry and the file nested inside it
		// (archive/<FolderName>/ and archive/<FolderName>/<file>)
		zipBytes := []byte(resp.Body().Raw())
		z, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
		require.NoError(t, err)
		var names []string
		for _, f := range z.File {
			names = append(names, f.Name)
		}
		require.Contains(t, names, "archive/Meetings/")
		require.Contains(t, names, "archive/Meetings/Checklist.txt")
	})
}

func TestSharedDriveFileCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("CanCopySharedFile", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		// Copy without name
		attrs := eB.POST("/sharings/drives/"+env.firstSharingID+"/"+env.checklistID+"/copy").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithBytes([]byte("")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes").Object()

		attrs.Value("name").String().IsEqual("Checklist (copy).txt")
		attrs.Value("driveId").String().IsEqual(env.firstSharingID)
	})

	t.Run("CopyWithSameName", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.POST("/sharings/drives/"+env.firstSharingID+"/"+env.checklistID+"/copy").
			WithQuery("Name", "Checklist.txt").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithBytes([]byte("")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.name").String().Contains("(")
	})

	t.Run("CopyWithCustomName", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.POST("/sharings/drives/"+env.firstSharingID+"/"+env.checklistID+"/copy").
			WithQuery("Name", "MyCustomCopy.txt").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithBytes([]byte("")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.name").String().IsEqual("MyCustomCopy.txt")
	})

	t.Run("CannotCopyUnsharedFile", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Create a file outside the shared drive
		nonSharedID := createFile(t, eA, env.outsideOfShareID, "NotShared.txt", env.acmeToken)

		eB.POST("/sharings/drives/"+env.firstSharingID+"/"+nonSharedID+"/copy").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithBytes([]byte("")).
			Expect().Status(403)
	})
}

func TestSharedDriveMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("GetMetadataByPath", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		// GET metadata request with file path (path includes root dir name)
		filePath := "/" + env.rootDirName + "/Meetings/Checklist.txt"
		obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/metadata").
			WithQuery("Path", filePath).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.Value("type").String().IsEqual("io.cozy.files")
		data.Value("id").String().IsEqual(env.checklistID)
		attrs := data.Value("attributes").Object()
		attrs.Value("type").String().IsEqual("file")
		attrs.Value("name").String().IsEqual("Checklist.txt")
	})

	t.Run("GetMetadataByDirectoryPath", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		// Path includes root dir name
		dirPath := "/" + env.rootDirName + "/Meetings"
		obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/metadata").
			WithQuery("Path", dirPath).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.Value("type").String().IsEqual("io.cozy.files")
		data.Value("id").String().IsEqual(env.meetingsDirID)
		attrs := data.Value("attributes").Object()
		attrs.Value("type").String().IsEqual("directory")
		attrs.Value("name").String().IsEqual("Meetings")
	})

	t.Run("GetMetadataNonExistent", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		nonExistentPath := "/" + env.rootDirName + "/NonExistent"
		eB.GET("/sharings/drives/"+env.firstSharingID+"/metadata").
			WithQuery("Path", nonExistentPath).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(404)
	})

	t.Run("GetMetadataWithoutPath", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.GET("/sharings/drives/"+env.firstSharingID+"/metadata").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(400)
	})

	t.Run("GetMetadataWithoutAuth", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		filePath := "/" + env.rootDirName + "/Meetings/Checklist.txt"
		eB.GET("/sharings/drives/"+env.firstSharingID+"/metadata").
			WithQuery("Path", filePath).
			Expect().Status(401)
	})

	t.Run("GetDirSize", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/"+env.firstRootDirID+"/size").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Accept", "application/vnd.api+json").
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.Value("id").IsEqual(env.firstRootDirID)
		data.Value("type").IsEqual("io.cozy.files.sizes")
		// Size should be at least 3 bytes (from Checklist.txt)
		data.Value("attributes").Object().Value("size").NotNull()
	})

	t.Run("MoveFileWithinDrive", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Create a file to move
		fileID := createFile(t, eA, env.meetingsDirID, "FileToMove.txt", env.acmeToken)

		// Move the file to product directory
		movedFile := eB.PATCH("/sharings/drives/"+env.firstSharingID+"/"+fileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.files",
					"id": "` + fileID + `",
					"attributes": {
						"dir_id": "` + env.productDirID + `"
					}
				}
			}`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		movedFile.Path("$.data.attributes.dir_id").String().IsEqual(env.productDirID)
	})

	t.Run("CannotMoveRootDirectory", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.PATCH("/sharings/drives/"+env.firstSharingID+"/"+env.firstRootDirID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.files",
					"id": "` + env.firstRootDirID + `",
					"attributes": {
						"dir_id": "` + env.productDirID + `"
					}
				}
			}`)).
			Expect().Status(422)
	})

	t.Run("CannotMoveFileOutsideDrive", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Create a file to attempt moving outside
		fileID := createFile(t, eA, env.meetingsDirID, "CannotMoveOut.txt", env.acmeToken)

		// Attempt to move outside the shared drive - should fail
		eB.PATCH("/sharings/drives/"+env.firstSharingID+"/"+fileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.files",
					"id": "` + fileID + `",
					"attributes": {
						"dir_id": "` + env.outsideOfShareID + `"
					}
				}
			}`)).
			Expect().Status(403)
	})

	t.Run("RenameFile", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Create a file to rename
		fileID := createFile(t, eA, env.meetingsDirID, "OriginalName.txt", env.acmeToken)

		// Rename the file
		renamed := eB.PATCH("/sharings/drives/"+env.firstSharingID+"/"+fileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.files",
					"id": "` + fileID + `",
					"attributes": {
						"name": "RenamedFile.txt"
					}
				}
			}`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		renamed.Path("$.data.attributes.name").String().IsEqual("RenamedFile.txt")
	})

	t.Run("OwnerCanRenameDriveRootViaFilesPatch", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		newDriveName := testify(t, "Renamed shared drive via files patch")

		renamed := eA.PATCH("/files/"+env.firstRootDirID).
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.files",
					"id": "` + env.firstRootDirID + `",
					"attributes": {
						"name": "` + newDriveName + `"
					}
				}
			}`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		renamed.Path("$.data.attributes.name").String().IsEqual(newDriveName)

		require.Eventually(t, func() bool {
			ownerSharing, err := sharing.FindSharing(env.acme, env.firstSharingID)
			if err != nil {
				return false
			}
			if ownerSharing.Description != newDriveName ||
				len(ownerSharing.Rules) == 0 || ownerSharing.Rules[0].Title != newDriveName {
				return false
			}
			recipientSharing, err := sharing.FindSharing(env.betty, env.firstSharingID)
			if err != nil {
				return false
			}
			return recipientSharing.Description == newDriveName &&
				len(recipientSharing.Rules) > 0 && recipientSharing.Rules[0].Title == newDriveName
		}, 5*time.Second, 50*time.Millisecond)
	})

	t.Run("OwnerCanPatchDriveDescription", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		newDriveName := testify(t, "Renamed shared drive via sharing patch")

		eA.PATCH("/sharings/"+env.firstSharingID).
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
				"data": {
					"type": "` + consts.Sharings + `",
					"attributes": {
						"description": "` + newDriveName + `"
					}
				}
			}`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.description").String().IsEqual(newDriveName)

		require.Eventually(t, func() bool {
			ownerSharing, err := sharing.FindSharing(env.acme, env.firstSharingID)
			if err != nil {
				return false
			}
			if ownerSharing.Description != newDriveName ||
				len(ownerSharing.Rules) == 0 || ownerSharing.Rules[0].Title != newDriveName {
				return false
			}
			recipientSharing, err := sharing.FindSharing(env.betty, env.firstSharingID)
			if err != nil {
				return false
			}
			return recipientSharing.Description == newDriveName &&
				len(recipientSharing.Rules) > 0 && recipientSharing.Rules[0].Title == newDriveName
		}, 5*time.Second, 50*time.Millisecond)
	})
}

func TestSharedDriveMultipleAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	// Create a second shared drive
	secondSharingID, secondRootDirID, _ := createSharedDrive(
		t,
		DriveCreationMethodLegacy,
		env.acme,
		env.acmeToken,
		env.tsA.URL,
		"Second Shared Drive",
		"Another drive for testing",
		nil,
	)
	acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

	// Create a file in the second drive
	eA, _, _ := env.createClients(t)
	fileID := createFile(t, eA, secondRootDirID, "SecondDriveFile.txt", env.acmeToken)

	t.Run("BettyCanAccessBothDrives", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		// Access first drive
		eB.GET("/sharings/drives/"+env.firstSharingID+"/"+env.checklistID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)

		// Access second drive
		eB.GET("/sharings/drives/"+secondSharingID+"/"+fileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
	})

	t.Run("ListShowsBothDrives", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		obj := eB.GET("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().Ge(2) // At least two drives
	})

	t.Run("CannotAccessWrongDrive", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		// Try to access file from first drive using second drive's ID
		eB.GET("/sharings/drives/"+secondSharingID+"/"+env.checklistID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(403)
	})
}

func TestSharedDriveNotes(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("CreateNoteInSharedDrive", func(t *testing.T) {
		eA, _, _ := env.createClients(t)

		obj := eA.POST("/sharings/drives/"+env.firstSharingID+"/notes").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.notes.documents",
					"attributes": {
						"title": "Test Note",
						"dir_id": "` + env.meetingsDirID + `",
						"schema": {
							"nodes": [
								["doc", { "content": "block+" }],
								["paragraph", { "content": "inline*", "group": "block" }],
								["text", { "group": "inline" }]
							],
							"marks": [["em", {}], ["strong", {}]],
							"topNode": "doc"
						}
					}
				}
			}`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.files")
		data.Value("id").String().NotEmpty()

		attrs := data.Value("attributes").Object()
		attrs.HasValue("type", "file")
		attrs.Value("name").String().Contains(".cozy-note")
		attrs.HasValue("mime", "text/vnd.cozy.note+markdown")
	})

	t.Run("OpenNoteFromSharedDrive", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// First create a note
		noteObj := eA.POST("/sharings/drives/"+env.firstSharingID+"/notes").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.notes.documents",
					"attributes": {
						"title": "Note to Open",
						"dir_id": "` + env.meetingsDirID + `",
						"schema": {
							"nodes": [
								["doc", { "content": "block+" }],
								["paragraph", { "content": "inline*", "group": "block" }],
								["text", { "group": "inline" }]
							],
							"marks": [],
							"topNode": "doc"
						}
					}
				}
			}`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		noteID := noteObj.Value("data").Object().Value("id").String().Raw()

		// Open the note as Betty (who has write access, ReadOnly: false)
		obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/notes/"+noteID+"/open").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", consts.NotesURL)
		data.HasValue("id", noteID)

		attrs := data.Value("attributes").Object()
		attrs.HasValue("note_id", noteID)
		sharecode := attrs.Value("sharecode").String().NotEmpty().Raw()

		permObj := eA.GET("/permissions/self").
			WithHeader("Authorization", "Bearer "+sharecode).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// The permissions should include write verbs (POST, PUT, PATCH, DELETE), not just GET
		permAttrs := permObj.Value("data").Object().Value("attributes").Object()
		perms := permAttrs.Value("permissions").Object()
		for _, rule := range perms.Iter() {
			verbs := rule.Object().Value("verbs").Array()
			verbs.Length().Gt(1)
			break
		}
	})

	t.Run("OpenFileWithEditorFromSharedDrive", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		fileID := createFileWithMime(t, eA, env.meetingsDirID, "drawing.excalidraw", env.acmeToken, "application/json")

		obj := eB.GET("/sharings/drives/"+env.firstSharingID+"/editor/"+fileID+"/open").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.ValueEqual("type", consts.Files)
		data.ValueEqual("id", fileID)

		attrs := data.Value("attributes").Object()
		attrs.ValueEqual("file_id", fileID)
		attrs.Value("instance").String().NotEmpty()
		attrs.Value("sharecode").String().NotEmpty()
	})

	t.Run("CreateNoteWithoutAuth", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.POST("/sharings/drives/"+env.firstSharingID+"/notes").
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
				"data": {
					"type": "io.cozy.notes.documents",
					"attributes": {
						"title": "Unauthorized",
						"dir_id": "` + env.meetingsDirID + `"
					}
				}
			}`)).
			Expect().Status(401)
	})
}

func TestSharedDriveRealtime(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("ReceiveCreatedEvent", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		ws := eB.GET("/sharings/drives/" + env.firstSharingID + "/realtime").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, env.bettyToken))
		time.Sleep(50 * time.Millisecond)

		// Create a file as owner
		newFileID := createFile(t, eA, env.meetingsDirID, "RealtimeTest.txt", env.acmeToken)

		// Verify Betty receives the CREATED event
		obj := ws.Expect().TextMessage().JSON().Object()
		obj.HasValue("event", "CREATED")
		payload := obj.Value("payload").Object()
		payload.HasValue("type", consts.Files)
		payload.HasValue("id", newFileID)
	})

	t.Run("IgnoreUnrelatedEventForFileRootSharedDrive", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		rootFileID := createFile(t, eA, "", "RealtimeRootIgnore.txt", env.acmeToken)
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			"Realtime file-root shared drive",
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

		ws := eB.GET("/sharings/drives/" + sharingID + "/realtime").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, env.bettyToken))
		time.Sleep(50 * time.Millisecond)

		unrelatedFileID := createFile(t, eA, "", "RealtimeOutside.txt", env.acmeToken)
		realtime.GetHub().Publish(env.acme, realtime.EventUpdate, &vfs.FileDoc{
			Type:    consts.Files,
			DocID:   unrelatedFileID,
			DocName: "RealtimeOutsideRenamed.txt",
		}, nil)

		raw := ws.Raw()
		require.NoError(t, raw.SetReadDeadline(time.Now().Add(250*time.Millisecond)))
		_, _, err := raw.ReadMessage()
		require.Error(t, err)
		netErr, ok := err.(net.Error)
		require.True(t, ok, "expected a timeout while waiting for an unrelated file event")
		require.True(t, netErr.Timeout(), "expected a timeout while waiting for an unrelated file event")
	})

	t.Run("ReceiveUpdatedEventForFileRootSharedDrive", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		rootFileID := createFile(t, eA, "", "RealtimeRootUpdate.txt", env.acmeToken)
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			"Realtime file-root shared drive",
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

		ws := eB.GET("/sharings/drives/" + sharingID + "/realtime").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, env.bettyToken))
		time.Sleep(50 * time.Millisecond)

		raw := ws.Raw()
		require.NoError(t, raw.SetReadDeadline(time.Now().Add(5*time.Second)))

		realtime.GetHub().Publish(env.acme, realtime.EventUpdate, &vfs.FileDoc{
			Type:    consts.Files,
			DocID:   rootFileID,
			DocName: "RealtimeRootRenamed.txt",
		}, nil)

		var msg struct {
			Event   string `json:"event"`
			Payload struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Doc  struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"doc"`
			} `json:"payload"`
		}
		require.NoError(t, raw.ReadJSON(&msg))
		require.Equal(t, "UPDATED", msg.Event)
		require.Equal(t, consts.Files, msg.Payload.Type)
		require.Equal(t, rootFileID, msg.Payload.ID)
		require.Equal(t, "RealtimeRootRenamed.txt", msg.Payload.Doc.Name)
	})
}

func TestSharedDriveRevocation(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	// Create a new shared drive specifically for revocation testing
	// so we don't affect other tests
	sharingID, rootDirID, _ := createSharedDrive(
		t,
		DriveCreationMethodLegacy,
		env.acme,
		env.acmeToken,
		env.tsA.URL,
		"Revocation Test Drive",
		"Drive for testing revocation",
		nil,
	)
	acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

	// Create a test file in the drive
	eA, _, _ := env.createClients(t)
	fileID := createFile(t, eA, rootDirID, "RevocationTestFile.txt", env.acmeToken)

	t.Run("RecipientCanInitiallyAccess", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.GET("/sharings/drives/"+sharingID+"/"+fileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
	})

	t.Run("RemoveRecipient", func(t *testing.T) {
		eA, _, _ := env.createClients(t)

		eA.DELETE("/sharings/"+sharingID+"/recipients/1").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(204)
	})

	t.Run("RevokedRecipientCannotAccess", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		eB.GET("/sharings/drives/"+sharingID+"/"+fileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(403)

		eB.GET("/sharings/drives/"+sharingID+"/metadata").
			WithQuery("Path", "/RevocationTestFile.txt").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(403)

		eB.GET("/sharings/drives/"+sharingID+"/_changes").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(403)
	})

	t.Run("RemoveRemainingRecipient", func(t *testing.T) {
		eA, _, _ := env.createClients(t)

		eA.DELETE("/sharings/"+sharingID+"/recipients/2").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(204)
	})

	t.Run("OwnerSharingIsDeleted", func(t *testing.T) {
		require.Eventually(t, func() bool {
			_, err := sharing.FindSharing(env.acme, sharingID)
			return couchdb.IsNotFoundError(err)
		}, 5*time.Second, 100*time.Millisecond, "Owner's drive sharing document should be deleted after the last recipient is revoked")
	})

	t.Run("OwnerRootReferenceIsRemoved", func(t *testing.T) {
		requireNoDirSharingReference(t, env.acme, rootDirID, sharingID)
	})

	t.Run("OwnerRootCanBeSharedAgain", func(t *testing.T) {
		eA, _, _ := env.createClients(t)

		eA.POST("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
				"data": {
					"type": "%s",
					"attributes": {
						"description": "Recreated drive after recipient revocation",
						"folder_id": "%s"
					}
				}
			}`, consts.Sharings, rootDirID))).
			Expect().Status(http.StatusCreated)
	})

	t.Run("RevokeAllRecipientsDeletesOwnerSharing", func(t *testing.T) {
		allSharingID, allRootDirID, _ := createSharedDrive(
			t,
			DriveCreationMethodLegacy,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			"Revoke All Recipients Drive",
			"Drive for testing full revocation",
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, allSharingID)

		eA, _, _ := env.createClients(t)
		eA.DELETE("/sharings/"+allSharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(204)

		require.Eventually(t, func() bool {
			_, err := sharing.FindSharing(env.acme, allSharingID)
			return couchdb.IsNotFoundError(err)
		}, 5*time.Second, 100*time.Millisecond, "Owner's drive sharing document should be deleted after revoking all recipients")

		requireNoDirSharingReference(t, env.acme, allRootDirID, allSharingID)
	})
}

func TestFileRootSharedDriveLastRecipientRemovalCleansReference(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, _, _ := env.createClients(t)

	rootFileID := createFile(t, eA, "", "FileRootRevocationReference.txt", env.acmeToken)
	sharingID, _ := createFileRootSharedDrive(
		t,
		env.acme,
		env.acmeToken,
		env.tsA.URL,
		rootFileID,
		"File root drive for reference cleanup",
		[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
	)
	acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

	eA.DELETE("/sharings/"+sharingID+"/recipients/1").
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		Expect().Status(http.StatusNoContent)

	require.Eventually(t, func() bool {
		_, err := sharing.FindSharing(env.acme, sharingID)
		return couchdb.IsNotFoundError(err)
	}, 5*time.Second, 100*time.Millisecond, "Owner's file-root drive sharing document should be deleted after the last recipient is revoked")

	requireNoFileSharingReference(t, env.acme, rootFileID, sharingID)

	eA.POST("/sharings/drives").
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(fmt.Sprintf(`{
			"data": {
				"type": "%s",
				"attributes": {
					"description": "Recreated file root drive after recipient revocation",
					"file_id": "%s"
				}
			}
		}`, consts.Sharings, rootFileID))).
		Expect().Status(http.StatusCreated)
}

func TestOpeningPendingSharedDriveShortcutClearsNewsCounter(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)
	sharingID := createDirectRecipientDriveSharing(
		t,
		env.acme,
		eA,
		env.acmeToken,
		"Betty Pending",
		"betty@example.net",
		env.tsB.URL,
		"Pending Revocation Drive",
		"Pending drive for revocation counter",
	)

	recipientSharing := waitForSharingOnRecipient(t, env.betty, sharingID)
	require.NotEmpty(t, recipientSharing.ShortcutID)
	newShortcutID := recipientSharing.ShortcutID

	eB.GET("/sharings/news").
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(http.StatusOK).
		JSON().Object().
		Path("$.meta.count").Number().IsEqual(1)

	loginSharingRecipient(t, eB)
	FakeOwnerInstanceForSharing(t, env.betty, env.tsA.URL, sharingID)
	require.NotEmpty(t, recipientSharing.Credentials)
	require.NotEmpty(t, recipientSharing.Credentials[0].State)
	authorizeLink := fmt.Sprintf(
		"%s/auth/authorize/sharing?sharing_id=%s&state=%s",
		env.tsB.URL,
		sharingID,
		url.QueryEscape(recipientSharing.Credentials[0].State),
	)
	openSharingAuthorize(t, eB, authorizeLink, sharingID)

	recipientSharing, err := sharing.FindSharing(env.betty, sharingID)
	require.NoError(t, err)
	require.NotEmpty(t, recipientSharing.ShortcutID)
	seenShortcutID := recipientSharing.ShortcutID
	require.Equal(t, newShortcutID, seenShortcutID)

	eB.GET("/sharings/news").
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(http.StatusOK).
		JSON().Object().
		Path("$.meta.count").Number().IsEqual(0)

	require.Eventually(t, func() bool {
		count, err := sharing.CountNewShortcuts(env.betty)
		return err == nil && count == 0
	}, 5*time.Second, 100*time.Millisecond, "Betty's sharing counter should not include the opened pending drive")

	require.Eventually(t, func() bool {
		_, err := env.betty.VFS().FileByID(newShortcutID)
		return err == nil
	}, 5*time.Second, 100*time.Millisecond, "Betty's opened shared-drive shortcut should still exist before revocation")

	eA.DELETE("/sharings/"+sharingID+"/recipients/1").
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		Expect().Status(http.StatusNoContent)

	require.Eventually(t, func() bool {
		_, err := env.betty.VFS().FileByID(newShortcutID)
		return os.IsNotExist(err)
	}, 5*time.Second, 100*time.Millisecond, "Betty's opened shared-drive shortcut should be removed after revocation")
}

func TestBackgroundRealtimeConnectionKeepsSharingNew(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)
	sharingID := createDirectRecipientDriveSharing(
		t,
		env.acme,
		eA,
		env.acmeToken,
		"Betty Background",
		"betty@example.net",
		env.tsB.URL,
		"Background Realtime Drive",
		"Drive watched by a background indexer",
	)

	recipientSharing := waitForSharingOnRecipient(t, env.betty, sharingID)
	require.NotEmpty(t, recipientSharing.ShortcutID)
	shortcutID := recipientSharing.ShortcutID

	loginSharingRecipient(t, eB)
	FakeOwnerInstanceForSharing(t, env.betty, env.tsA.URL, sharingID)
	require.NotEmpty(t, recipientSharing.Credentials)
	require.NotEmpty(t, recipientSharing.Credentials[0].State)
	authorizeLink := fmt.Sprintf(
		"%s/auth/authorize/sharing?sharing_id=%s&state=%s",
		env.tsB.URL,
		sharingID,
		url.QueryEscape(recipientSharing.Credentials[0].State),
	)
	openSharingAuthorize(t, eB, authorizeLink, sharingID)

	// Put the shortcut back in the "new" state, like after an auto-accepted
	// sharing where the user has not looked at the drive yet.
	setShortcutSharingStatus(t, env.betty, shortcutID, "new")

	ws := eB.GET("/sharings/drives/"+sharingID+"/realtime").
		WithQuery("background", "true").
		WithWebsocketUpgrade().
		Expect().Status(http.StatusSwitchingProtocols).
		Websocket()
	ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, env.bettyToken))
	time.Sleep(300 * time.Millisecond)
	ws.Disconnect()

	count, err := sharing.CountNewShortcuts(env.betty)
	require.NoError(t, err)
	require.Equal(t, 1, count, "a background realtime connection should not mark the sharing as seen")

	ws = eB.GET("/sharings/drives/" + sharingID + "/realtime").
		WithWebsocketUpgrade().
		Expect().Status(http.StatusSwitchingProtocols).
		Websocket()
	defer ws.Disconnect()
	ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, env.bettyToken))

	require.Eventually(t, func() bool {
		count, err := sharing.CountNewShortcuts(env.betty)
		return err == nil && count == 0
	}, 5*time.Second, 100*time.Millisecond, "a foreground realtime connection should mark the sharing as seen")
}

func setShortcutSharingStatus(t *testing.T, inst *instance.Instance, shortcutID, status string) {
	t.Helper()
	file, err := inst.VFS().FileByID(shortcutID)
	require.NoError(t, err)
	old := file.Clone().(*vfs.FileDoc)
	meta, ok := file.Metadata["sharing"].(map[string]interface{})
	require.True(t, ok, "shortcut should carry sharing metadata")
	meta["status"] = status
	require.NoError(t, inst.VFS().UpdateFileDoc(old, file))
}

func TestDirectoryRootSharedDriveOwnerDeletionRevokesRecipient(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)

	sharingID, rootDirID, _ := createSharedDrive(
		t,
		DriveCreationMethodFromFolder,
		env.acme,
		env.acmeToken,
		env.tsA.URL,
		"Deleted Root Drive",
		"Drive deleted by owner",
		nil,
	)

	eOwnerSharing := newSharingExpect(t, env.tsA.URL)
	eBettySharing := newSharingExpect(t, env.tsB.URL)
	loginSharingRecipient(t, eBettySharing)
	authorizeLink := prepareSharingAuthorizeLink(t, env.acme, "Betty", sharingID, eOwnerSharing, env.tsB.URL)
	FakeOwnerInstanceForSharing(t, env.betty, env.tsA.URL, sharingID)
	openSharingAuthorize(t, eBettySharing, authorizeLink, sharingID)
	waitForDriveSharingReadyOnOwner(t, eOwnerSharing, env.acmeToken, sharingID)
	waitForDriveSharingActiveOnRecipient(t, env.betty, sharingID)

	recipientSharing, err := sharing.FindSharing(env.betty, sharingID)
	require.NoError(t, err)
	require.NotEmpty(t, recipientSharing.ShortcutID)
	shortcutID := recipientSharing.ShortcutID
	_, err = env.betty.VFS().FileByID(shortcutID)
	require.NoError(t, err)

	sharedDriveVisibleForBetty := func() bool {
		obj := eB.GET("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(http.StatusOK).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		for _, item := range obj.Value("data").Array().Iter() {
			if item.Object().Value("id").String().Raw() == sharingID {
				return true
			}
		}
		return false
	}

	require.True(t, sharedDriveVisibleForBetty(), "Betty should initially see the accepted shared drive")

	eB.GET("/sharings/drives/"+sharingID+"/"+rootDirID).
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(http.StatusOK)

	eA.DELETE("/files/"+rootDirID).
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		Expect().Status(http.StatusOK)

	require.Eventually(t, func() bool {
		return !sharedDriveVisibleForBetty()
	}, 5*time.Second, 100*time.Millisecond, "Betty should no longer see a shared drive after its owner deletes the root folder")

	require.Eventually(t, func() bool {
		_, err := sharing.FindSharing(env.acme, sharingID)
		return couchdb.IsNotFoundError(err)
	}, 5*time.Second, 100*time.Millisecond, "Owner's sharing document should be deleted after deleting the shared-drive root folder")

	require.Eventually(t, func() bool {
		_, err := env.betty.VFS().FileByID(shortcutID)
		return os.IsNotExist(err)
	}, 5*time.Second, 100*time.Millisecond, "Betty's shared-drive shortcut should be removed after the owner deletes the root folder")
}

func TestFileRootSharedDriveOwnerDeletionRevokesRecipient(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)

	rootFileID := createFile(t, eA, "", "Deleted Root File.txt", env.acmeToken)
	sharingID, _ := createFileRootSharedDrive(
		t,
		env.acme,
		env.acmeToken,
		env.tsA.URL,
		rootFileID,
		"File drive deleted by owner",
		[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
	)
	acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)
	waitForDriveSharingReadyOnOwner(t, eA, env.acmeToken, sharingID)
	waitForDriveSharingActiveOnRecipient(t, env.betty, sharingID)

	sharedDriveVisibleForBetty := func() bool {
		obj := eB.GET("/sharings/drives").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(http.StatusOK).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		for _, item := range obj.Value("data").Array().Iter() {
			if item.Object().Value("id").String().Raw() == sharingID {
				return true
			}
		}
		return false
	}

	require.True(t, sharedDriveVisibleForBetty(), "Betty should initially see the accepted file-root shared drive")

	eB.GET("/sharings/drives/"+sharingID+"/"+rootFileID).
		WithHeader("Authorization", "Bearer "+env.bettyToken).
		Expect().Status(http.StatusOK)

	eA.DELETE("/files/"+rootFileID).
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		Expect().Status(http.StatusOK)

	require.Eventually(t, func() bool {
		return !sharedDriveVisibleForBetty()
	}, 5*time.Second, 100*time.Millisecond, "Betty should no longer see a file-root shared drive after its owner deletes the root file")

	require.Eventually(t, func() bool {
		_, err := sharing.FindSharing(env.acme, sharingID)
		return couchdb.IsNotFoundError(err)
	}, 5*time.Second, 100*time.Millisecond, "Owner's sharing document should be deleted after deleting the shared-drive root file")
}

// TestSharedDriveRecipientSelfRevocation tests that a recipient can revoke themselves
// from a shared drive and the owner is properly notified.
func TestSharedDriveRecipientSelfRevocation(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)

	// Use the existing shared drive from setupSharedDrivesEnv where Betty is already a member
	sharingID := env.firstSharingID

	t.Run("VerifyBettyIsMember", func(t *testing.T) {
		obj := eA.GET("/sharings/"+sharingID).
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Betty should be a ready member (member index 1, after owner)
		members := obj.Path("$.data.attributes.members").Array()
		members.Value(1).Object().HasValue("status", "ready")
		members.Value(1).Object().HasValue("name", "Betty")
	})

	t.Run("BettyRevokesHerself", func(t *testing.T) {
		// Betty revokes herself from the sharing
		eB.DELETE("/sharings/"+sharingID+"/recipients/self").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(204)
	})

	t.Run("VerifyBettyIsRevokedOnOwnerSide", func(t *testing.T) {
		// Wait for the revocation notification to be processed on owner's side
		require.Eventually(t, func() bool {
			obj := eA.GET("/sharings/"+sharingID).
				WithHeader("Authorization", "Bearer "+env.acmeToken).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			status := obj.Path("$.data.attributes.members[1].status").String().Raw()
			return status == "revoked"
		}, 5*time.Second, 100*time.Millisecond, "Betty's status should be revoked on owner's side")
	})

	t.Run("OwnerCanAddBettyAgain", func(t *testing.T) {
		eA.GET("/files/"+env.firstRootDirID).
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(http.StatusOK)

		bettyContact, err := contact.FindByEmail(env.acme, "betty@example.net")
		require.NoError(t, err)

		eA.POST("/sharings/"+sharingID+"/recipients").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes(makeAddRecipientsPayload(t, sharingID, "recipients", bettyContact.ID())).
			Expect().Status(http.StatusOK)

		ownerSharing, err := sharing.FindSharing(env.acme, sharingID)
		require.NoError(t, err)

		bettyCount := 0
		for index, member := range ownerSharing.Members {
			if member.Email != "betty@example.net" {
				continue
			}
			bettyCount++
			require.Equal(t, 1, index)
			require.Equal(t, sharing.MemberStatusPendingInvitation, member.Status)
			require.False(t, member.ReadOnly)
		}
		require.Equal(t, 1, bettyCount)
		require.NotEmpty(t, ownerSharing.Credentials[0].State)
	})
}

// TestSharedDriveGroupDynamicMembership tests that when a contact is added to a group
// that is part of a shared drive, the new member automatically gets access to the drive
// without needing to accept a specific invitation.
func TestSharedDriveGroupDynamicMembership(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	cfg := config.GetConfig()
	cfg.Assets = "../../assets"
	cfg.Contexts = map[string]interface{}{
		config.DefaultInstanceContext: map[string]interface{}{
			"sharing": map[string]interface{}{
				"auto_accept_trusted_contacts": true,
				"auto_accept_trusted":          true,
				"trusted_domains":              []interface{}{"cozy.local", "example.com"},
			},
		},
	}
	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()
	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()))

	// Create owner instance (ACME)
	ownerSetup := testutils.NewSetup(t, t.Name()+"_owner")
	ownerInstance := ownerSetup.GetTestInstance(&lifecycle.Options{
		Email:      "owner@example.com",
		PublicName: "Owner",
	})
	ownerAppToken := generateAppToken(ownerInstance, "drive", consts.Files)
	tsOwner := ownerSetup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/files":    files.Routes,
		"/sharings": sharings.Routes,
	})
	tsOwner.Config.Handler.(*echo.Echo).Renderer = render
	tsOwner.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsOwner.Close)

	// Create first group member instance (Alice - already in the group when drive is created)
	aliceSetup := testutils.NewSetup(t, t.Name()+"_alice")
	aliceInstance := aliceSetup.GetTestInstance(&lifecycle.Options{
		Email:         "alice@example.com",
		PublicName:    "Alice",
		Passphrase:    "MyPassphrase",
		KdfIterations: 5000,
		Key:           "xxx",
	})
	aliceAppToken := generateAppToken(aliceInstance, "drive", consts.Files)
	tsAlice := aliceSetup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/auth": func(g *echo.Group) {
			g.Use(middlewares.LoadSession)
			auth.Routes(g)
		},
		"/files":    files.Routes,
		"/sharings": sharings.Routes,
	})
	tsAlice.Config.Handler.(*echo.Echo).Renderer = render
	tsAlice.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsAlice.Close)

	// Create new member instance (Bob - will be added to the group later)
	bobSetup := testutils.NewSetup(t, t.Name()+"_bob")
	bobInstance := bobSetup.GetTestInstance(&lifecycle.Options{
		Email:         "bob@example.com",
		PublicName:    "Bob",
		Passphrase:    "MyPassphrase",
		KdfIterations: 5000,
		Key:           "xxx",
	})
	bobAppToken := generateAppToken(bobInstance, "drive", consts.Files)
	tsBob := bobSetup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/auth": func(g *echo.Group) {
			g.Use(middlewares.LoadSession)
			auth.Routes(g)
		},
		"/files":    files.Routes,
		"/sharings": sharings.Routes,
	})
	tsBob.Config.Handler.(*echo.Echo).Renderer = render
	tsBob.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(tsBob.Close)

	eOwner := httpexpect.Default(t, tsOwner.URL)

	// Step 1: Create a group on owner's instance
	teamGroup := createGroupOnInstance(t, ownerInstance, "Team")

	// Step 2: Create Alice as a contact in that group on owner's instance (with Cozy URL for auto-accept)
	aliceContact := createContactInGroupWithCozy(t, ownerInstance, teamGroup, "Alice", "alice@example.com", tsAlice.URL)
	require.NotNil(t, aliceContact)

	// Step 3: Create a directory for the shared drive
	sharedDirID := createRootDirectory(t, eOwner, "Team Drive", ownerAppToken)

	// Step 4: Create a file in the directory before sharing
	fileID := createFile(t, eOwner, sharedDirID, "TeamDocument.txt", ownerAppToken)

	// Step 5: Create a shared drive with the group as recipient
	obj := eOwner.POST("/sharings/drives").
		WithHeader("Authorization", "Bearer "+ownerAppToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{
			"data": {
				"type": "` + consts.Sharings + `",
				"attributes": {
					"description": "Team Drive for dynamic membership test",
					"folder_id": "` + sharedDirID + `"
				},
				"relationships": {
					"recipients": {
						"data": [{"id": "` + teamGroup.ID() + `", "type": "` + consts.Groups + `"}]
					}
				}
			}
		}`)).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()

	sharingID := obj.Value("data").Object().Value("id").String().NotEmpty().Raw()

	// Verify group and members are set correctly
	attrs := obj.Value("data").Object().Value("attributes").Object()
	groups := attrs.Value("groups").Array()
	groups.Length().IsEqual(1)
	groups.Value(0).Object().Value("id").IsEqual(teamGroup.ID())
	groups.Value(0).Object().Value("name").IsEqual("Team")

	members := attrs.Value("members").Array()
	members.Length().IsEqual(2) // Owner + Alice
	members.Value(1).Object().Value("name").IsEqual("Alice")
	members.Value(1).Object().Value("only_in_groups").IsEqual(true)
	members.Value(1).Object().Value("groups").Array().Value(0).IsEqual(0)

	// Step 6: Wait for the sharing to exist on Alice's instance, then set up the owner's URL
	eAlice := httpexpect.Default(t, tsAlice.URL)
	waitForSharingOnRecipientWithOwnerURL(t, aliceInstance, sharingID, tsOwner.URL)
	waitForAutoAcceptJobForSharing(t, aliceInstance, sharingID)

	// Step 7: Wait for Alice's sharing to be auto-accepted
	require.Eventually(t, func() bool {
		var s sharing.Sharing
		if err := couchdb.GetDoc(aliceInstance, consts.Sharings, sharingID, &s); err != nil {
			return false
		}
		return s.Active
	}, 10*time.Second, 500*time.Millisecond, "Alice's sharing should be auto-accepted and active")

	// Step 8: Verify Alice can access the file
	eAlice.GET("/sharings/drives/"+sharingID+"/"+fileID).
		WithHeader("Authorization", "Bearer "+aliceAppToken).
		Expect().Status(200)

	// Step 9: Create Bob as a contact WITHOUT the group initially (with Cozy URL for auto-accept)
	bobContact := createContactWithCozy(t, ownerInstance, "Bob", "bob@example.com", tsBob.URL)

	// Step 10: Add Bob to the group by updating the contact
	addContactToExistingGroup(t, ownerInstance, bobContact, teamGroup)

	// Step 11: Call UpdateGroups to simulate the trigger that fires when contact group membership changes
	msg := job.ShareGroupMessage{
		ContactID:   bobContact.ID(),
		GroupsAdded: []string{teamGroup.ID()},
	}
	require.NoError(t, sharing.UpdateGroups(ownerInstance, msg))

	// Step 12: Reload the sharing to verify Bob was added
	var s sharing.Sharing
	require.NoError(t, couchdb.GetDoc(ownerInstance, consts.Sharings, sharingID, &s))
	require.Len(t, s.Members, 3, "Should now have 3 members: Owner, Alice, Bob")
	require.Equal(t, "Bob", s.Members[2].Name)
	require.True(t, s.Members[2].OnlyInGroups)
	require.Equal(t, []int{0}, s.Members[2].Groups)

	// Step 13: Wait for the sharing to exist on Bob's instance, then set up the owner's URL
	eBob := httpexpect.Default(t, tsBob.URL)
	waitForSharingOnRecipientWithOwnerURL(t, bobInstance, sharingID, tsOwner.URL)
	waitForAutoAcceptJobForSharing(t, bobInstance, sharingID)

	// Step 14: Verify Bob can access the shared drive after auto-accept
	// Wait for the sharing to be auto-accepted on Bob's side
	require.Eventually(t, func() bool {
		var s sharing.Sharing
		if err := couchdb.GetDoc(bobInstance, consts.Sharings, sharingID, &s); err != nil {
			return false
		}
		return s.Active
	}, 10*time.Second, 500*time.Millisecond, "Bob's sharing should be auto-accepted and active")

	// Step 15: Verify Bob can access the file in the shared drive
	eBob.GET("/sharings/drives/"+sharingID+"/"+fileID).
		WithHeader("Authorization", "Bearer "+bobAppToken).
		Expect().Status(200)

	// Step 16: Verify Bob can see the file content via download
	res := eBob.GET("/sharings/drives/"+sharingID+"/download/"+fileID).
		WithHeader("Authorization", "Bearer "+bobAppToken).
		Expect().Status(200)
	res.Body().IsEqual("foo")
}

// createGroupOnInstance creates a contact group on the specified instance.
func createGroupOnInstance(t *testing.T, inst *instance.Instance, name string) *contact.Group {
	t.Helper()
	g := contact.NewGroup()
	g.M["name"] = name
	require.NoError(t, couchdb.CreateDoc(inst, g))
	return g
}

// createContactInGroupWithCozy creates a contact in a group with a Cozy URL.
func createContactInGroupWithCozy(t *testing.T, inst *instance.Instance, g *contact.Group, name, email, cozyURL string) *contact.Contact {
	t.Helper()
	mail := map[string]interface{}{"address": email}
	c := contact.New()
	c.M["fullname"] = name
	c.M["email"] = []interface{}{mail}
	c.M["cozy"] = []interface{}{
		map[string]interface{}{"url": cozyURL, "primary": true},
	}
	c.M["relationships"] = map[string]interface{}{
		"groups": map[string]interface{}{
			"data": []interface{}{
				map[string]interface{}{
					"_id":   g.ID(),
					"_type": consts.Groups,
				},
			},
		},
	}
	require.NoError(t, couchdb.CreateDoc(inst, c))
	return c
}

// createContactWithCozy creates a contact with a Cozy URL but NOT in any group.
func createContactWithCozy(t *testing.T, inst *instance.Instance, name, email, cozyURL string) *contact.Contact {
	t.Helper()
	mail := map[string]interface{}{"address": email}
	c := contact.New()
	c.M["fullname"] = name
	c.M["email"] = []interface{}{mail}
	c.M["cozy"] = []interface{}{
		map[string]interface{}{"url": cozyURL, "primary": true},
	}
	require.NoError(t, couchdb.CreateDoc(inst, c))
	return c
}

// addContactToExistingGroup adds an existing contact to a group by updating the contact.
func addContactToExistingGroup(t *testing.T, inst *instance.Instance, c *contact.Contact, g *contact.Group) {
	t.Helper()

	// Get existing groups or create empty slice
	var existingGroups []interface{}
	if rels, ok := c.M["relationships"].(map[string]interface{}); ok {
		if groups, ok := rels["groups"].(map[string]interface{}); ok {
			if data, ok := groups["data"].([]interface{}); ok {
				existingGroups = data
			}
		}
	}

	// Add the new group
	existingGroups = append(existingGroups, map[string]interface{}{
		"_id":   g.ID(),
		"_type": consts.Groups,
	})

	// Update the contact
	c.M["relationships"] = map[string]interface{}{
		"groups": map[string]interface{}{
			"data": existingGroups,
		},
	}
	require.NoError(t, couchdb.UpdateDoc(inst, c))
}
