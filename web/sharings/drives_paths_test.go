package sharings_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func patchDirName(t *testing.T, e *httpexpect.Expect, token, id, name string) {
	t.Helper()
	e.PATCH("/files/"+id).
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{"data": {"type": "io.cozy.files", "id": "` + id + `", "attributes": {"name": "` + name + `"}}}`)).
		Expect().Status(200)
}

func TestSharedDriveRelativePaths(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, _, _ := env.createClients(t)

	// Put the root of the drive under private folders of the owner
	privateID := createRootDirectory(t, eA, "Private", env.acmeToken)
	clientsID := createDirectory(t, eA, privateID, "Clients", env.acmeToken)
	eA.PATCH("/files/"+env.firstRootDirID).
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{"data": {"type": "io.cozy.files", "id": "` + env.firstRootDirID + `", "attributes": {"dir_id": "` + clientsID + `"}}}`)).
		Expect().Status(200)

	drive := "/sharings/drives/" + env.firstSharingID
	root := "/" + env.rootDirName

	get := func(eB *httpexpect.Expect, url string, query map[string]string) *httpexpect.Response {
		req := eB.GET(url).WithHeader("Authorization", "Bearer "+env.bettyToken)
		for k, v := range query {
			req = req.WithQuery(k, v)
		}
		return req.Expect().Status(200)
	}
	noPrivateName := func(t *testing.T, body string) {
		t.Helper()
		assert.NotContains(t, body, "Private")
		assert.NotContains(t, body, "Clients")
	}

	t.Run("FolderAndItsContent", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		res := get(eB, drive+"/"+env.firstRootDirID, nil)
		noPrivateName(t, res.Body().Raw())
		obj := res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object()
		obj.Path("$.data.attributes.path").String().IsEqual(root)
		for _, inc := range obj.Value("included").Array().Iter() {
			inc.Object().Path("$.attributes.path").String().HasPrefix(root + "/")
		}

		res = get(eB, drive+"/"+env.meetingsDirID, nil)
		noPrivateName(t, res.Body().Raw())
		res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.path").String().IsEqual(root + "/Meetings")
	})

	t.Run("MetadataByPath", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		res := get(eB, drive+"/metadata", map[string]string{"Path": root + "/Meetings"})
		noPrivateName(t, res.Body().Raw())
		res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().IsEqual(env.meetingsDirID)

		eB.GET(drive+"/metadata").
			WithQuery("Path", "/Private/Clients"+root+"/Meetings").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(404)
		eB.GET(drive+"/metadata").
			WithQuery("Path", root+"/../Meetings").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(404)
	})

	t.Run("ReuseFilePath", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		feed := get(eB, drive+"/_changes", nil).JSON().Object().Value("results").Array()
		var p string
		for _, row := range feed.Iter() {
			if row.Object().Value("id").String().Raw() == env.checklistID {
				p = row.Object().Path("$.doc.path").String().IsEqual(root + "/Meetings/Checklist.txt").Raw()
				break
			}
		}
		require.NotEmpty(t, p)
		download := eB.POST(drive+"/downloads").WithQuery("Path", p).
			WithHeader("Authorization", "Bearer "+env.bettyToken).Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).Object().Path("$.links.related").String().Raw()
		get(eB, download, nil).Body().IsEqual("foo")

		archive := eB.POST(drive+"/archive").WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", jsonAPIContentType).
			WithBytes([]byte(fmt.Sprintf(`{"data":{"attributes":{"files":[%q]}}}`, p))).
			Expect().Status(200)
		noPrivateName(t, archive.Body().Raw())
		obj := archive.JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).Object()
		obj.Path("$.data.attributes.files[0]").String().IsEqual(p)
		body := []byte(get(eB, obj.Path("$.links.related").String().Raw(), nil).Body().Raw())
		z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		require.NoError(t, err)
		require.Len(t, z.File, 1)
		require.Equal(t, "archive/Checklist.txt", z.File[0].Name)
		f, err := z.File[0].Open()
		require.NoError(t, err)
		defer f.Close()
		content, err := io.ReadAll(f)
		require.NoError(t, err)
		require.Equal(t, "foo", string(content))
	})

	t.Run("CreationAndTrash", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		res := eB.POST(drive+"/"+env.meetingsDirID).
			WithQuery("Name", "Notes").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(201)
		noPrivateName(t, res.Body().Raw())
		res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.path").String().IsEqual(root + "/Meetings/Notes")

		res = eB.DELETE(drive+"/"+env.checklistID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
		noPrivateName(t, res.Body().Raw())
		res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.restore_path").String().IsEqual(root + "/Meetings")
	})

	t.Run("RenameAndRestore", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		draftsID := createDirectory(t, eA, env.meetingsDirID, "Drafts", env.acmeToken)
		createDirectory(t, eA, draftsID, "Sub", env.acmeToken)
		checkDir := func(res *httpexpect.Response, want string) {
			t.Helper()
			noPrivateName(t, res.Body().Raw())
			obj := res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object()
			obj.Path("$.data.attributes.path").String().IsEqual(want)
			included := obj.Value("included").Array()
			included.NotEmpty()
			for _, inc := range included.Iter() {
				inc.Object().Path("$.attributes.path").String().HasPrefix(want + "/")
			}
		}

		res := eB.PATCH(drive+"/"+draftsID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{"data": {"type": "io.cozy.files", "id": "` + draftsID + `", "attributes": {"name": "Drafts2"}}}`)).
			Expect().Status(200)
		checkDir(res, root+"/Meetings/Drafts2")
		checkDir(get(eB, drive+"/"+draftsID, nil), root+"/Meetings/Drafts2")

		eB.DELETE(drive+"/"+draftsID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
		res = eB.POST(drive+"/trash/"+draftsID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
		checkDir(res, root+"/Meetings/Drafts2")
	})

	t.Run("Changes", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		patchDirName(t, eA, env.acmeToken, clientsID, "Renamed")
		res := get(eB, drive+"/_changes", nil)
		noPrivateName(t, res.Body().Raw())
		assert.NotContains(t, res.Body().Raw(), "Renamed")

		var feed struct {
			Results []struct {
				ID  string `json:"id"`
				Doc struct {
					Path string `json:"path"`
				} `json:"doc"`
			} `json:"results"`
		}
		require.NoError(t, json.Unmarshal([]byte(res.Body().Raw()), &feed))
		found := false
		for _, r := range feed.Results {
			if r.ID == env.meetingsDirID {
				found = true
				assert.Equal(t, root+"/Meetings", r.Doc.Path)
			}
		}
		assert.True(t, found)
	})

	t.Run("RenameTheRoot", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		rename := func(name string) *httpexpect.Response {
			return eB.PATCH(drive+"/"+env.firstRootDirID).
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				WithHeader("Content-Type", "application/vnd.api+json").
				WithBytes([]byte(`{"data": {"type": "io.cozy.files", "id": "` + env.firstRootDirID + `", "attributes": {"name": "` + name + `"}}}`)).
				Expect().Status(200)
		}
		res := rename("Other root")
		noPrivateName(t, res.Body().Raw())
		res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.path").String().IsEqual("/Other root")
		rename(env.rootDirName)
	})

	t.Run("DownloadsAnswerTheSame", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		post := func(path string) string {
			return eB.POST(drive+"/downloads").
				WithQuery("Path", path).
				WithHeader("Authorization", "Bearer "+env.bettyToken).
				Expect().Status(404).Body().Raw()
		}
		outside := "/Private"
		assert.Equal(t, post("/Does not exist"), post(outside))

		fileID := createFile(t, eA, env.meetingsDirID, "Agenda.txt", env.acmeToken)
		eB.POST(drive+"/downloads").
			WithQuery("Id", fileID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
	})
}

func TestSharedDriveInteractCodePaths(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, _, _ := env.createClients(t)

	privateID := createRootDirectory(t, eA, "Private", env.acmeToken)
	clientsID := createDirectory(t, eA, privateID, "Clients", env.acmeToken)
	eA.PATCH("/files/"+env.firstRootDirID).
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{"data": {"type": "io.cozy.files", "id": "` + env.firstRootDirID + `", "attributes": {"dir_id": "` + clientsID + `"}}}`)).
		Expect().Status(200)

	code := getSharedDriveInteractCode(t, env.acme, env.firstSharingID, "betty@example.net")
	root := "/" + env.rootDirName

	t.Run("ByID", func(t *testing.T) {
		for id, expected := range map[string]string{
			env.firstRootDirID: root,
			env.meetingsDirID:  root + "/Meetings",
		} {
			res := eA.GET("/files/"+id).
				WithHeader("Authorization", "Bearer "+code).
				Expect().Status(200)
			assert.NotContains(t, res.Body().Raw(), "Private")
			assert.NotContains(t, res.Body().Raw(), "Clients")
			res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().Path("$.data.attributes.path").String().IsEqual(expected)
		}
	})
	t.Run("ByPath", func(t *testing.T) {
		res := eA.GET("/files/metadata").
			WithQuery("Path", root+"/Meetings").
			WithHeader("Authorization", "Bearer "+code).
			Expect().Status(200)
		assert.NotContains(t, res.Body().Raw(), "Private")
		assert.NotContains(t, res.Body().Raw(), "Clients")
		res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.path").String().IsEqual(root + "/Meetings")
	})
	t.Run("ByPathOutsideTheDrive", func(t *testing.T) {
		for _, p := range []string{"/Private/Clients" + root + "/Meetings", "/Private", root + "/../Other"} {
			eA.GET("/files/metadata").
				WithQuery("Path", p).
				WithHeader("Authorization", "Bearer "+code).
				Expect().Status(404)
		}
	})
}

func TestSharedDriveRecipientsOfANestedDrive(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, eD := env.createClients(t)

	parentName := "Parent Drive Above"
	d1ID, d1RootID, _ := createSharedDrive(t, DriveCreationMethodFromFolder,
		env.acme, env.acmeToken, env.tsA.URL, parentName, "d1",
		[]RecipientInfo{{Name: "Dave", Email: "dave@example.net", ReadOnly: false}})
	childID := createDirectory(t, eA, d1RootID, "Child", env.acmeToken)
	d2ID := createDriveOnFolder(t, eA, env.acme, childID, env.acmeToken,
		[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}})
	acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, d1ID)
	acceptSharedDrive(t, env.acme, env.betty, "Betty", env.tsA.URL, env.tsB.URL, d2ID)

	assertOnlyNestedDrive := func(t *testing.T, res *httpexpect.Response) {
		t.Helper()
		body := res.Body().Raw()
		assert.NotContains(t, body, parentName)
		assert.NotContains(t, body, "dave@example.net")
		assert.NotContains(t, body, d1ID)
		assert.Contains(t, body, "betty@example.net")
	}

	t.Run("NestedMemberThroughTheDriveRoute", func(t *testing.T) {
		res := eB.GET("/sharings/drives/"+d2ID+"/recipients/"+childID).
			WithHeader("Authorization", "Bearer "+env.bettyToken).
			Expect().Status(200)
		assertOnlyNestedDrive(t, res)
	})

	t.Run("NestedMemberWithTheInteractCode", func(t *testing.T) {
		code := getSharedDriveInteractCode(t, env.acme, d2ID, "betty@example.net")
		res := eA.GET("/sharings/recipients/"+childID).
			WithHeader("Authorization", "Bearer "+code).
			Expect().Status(200)
		assertOnlyNestedDrive(t, res)
	})

	t.Run("ParentMemberKeepsTheParentDrive", func(t *testing.T) {
		res := eD.GET("/sharings/drives/"+d1ID+"/recipients/"+childID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)
		body := res.Body().Raw()
		assert.Contains(t, body, parentName)
		assert.Contains(t, body, "dave@example.net")
		assert.Contains(t, body, "betty@example.net")
	})

	t.Run("OwnerSeesEveryone", func(t *testing.T) {
		res := eA.GET("/sharings/drives/"+d2ID+"/recipients/"+childID).
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(200)
		body := res.Body().Raw()
		assert.Contains(t, body, parentName)
		assert.Contains(t, body, "dave@example.net")
		assert.Contains(t, body, "betty@example.net")
	})
}

func TestSharedDriveLinkAndChildrenPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, _, _ := env.createClients(t)

	privateID := createRootDirectory(t, eA, "Private", env.acmeToken)
	clientsID := createDirectory(t, eA, privateID, "Clients", env.acmeToken)
	eA.PATCH("/files/"+env.firstRootDirID).
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithBytes([]byte(`{"data": {"type": "io.cozy.files", "id": "` + env.firstRootDirID + `", "attributes": {"dir_id": "` + clientsID + `"}}}`)).
		Expect().Status(200)
	notesID := createDirectory(t, eA, env.meetingsDirID, "Notes", env.acmeToken)

	root := "/" + env.rootDirName
	noPrivateName := func(t *testing.T, res *httpexpect.Response) *httpexpect.Object {
		t.Helper()
		assert.NotContains(t, res.Body().Raw(), "Private")
		assert.NotContains(t, res.Body().Raw(), "Clients")
		return res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object()
	}

	code := getSharedDriveInteractCode(t, env.acme, env.firstSharingID, "betty@example.net")
	t.Run("InteractCodeChildren", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		res := eA.GET("/files/"+env.firstRootDirID+"/relationships/contents").
			WithHeader("Authorization", "Bearer "+code).
			Expect().Status(200)
		obj := noPrivateName(t, res)
		paths := []string{}
		for _, child := range obj.Value("data").Array().Iter() {
			paths = append(paths, child.Object().Path("$.attributes.path").String().Raw())
		}
		assert.ElementsMatch(t, []string{root + "/Product", root + "/Meetings"}, paths)
	})

	payload := makeSharedDrivePermissionPayload(t, "io.cozy.files", []string{env.meetingsDirID}, "", nil)
	link := eA.POST("/permissions").WithQuery("codes", "link").
		WithHeader("Authorization", "Bearer "+env.acmeToken).
		WithHeader("Content-Type", jsonAPIContentType).
		WithBytes(payload).Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
		Object().Path("$.data.attributes.codes.link").String().NotEmpty().Raw()

	// The folder of the link is the root of the paths: the drive above it is
	// not for the visitor.
	t.Run("LinkOnTheDriveRoutes", func(t *testing.T) {
		eA, _, _ := env.createClients(t)
		drive := "/sharings/drives/" + env.firstSharingID
		hidden := func(t *testing.T, res *httpexpect.Response) *httpexpect.Object {
			t.Helper()
			assert.NotContains(t, res.Body().Raw(), env.rootDirName)
			return noPrivateName(t, res)
		}

		obj := hidden(t, eA.GET(drive+"/"+env.meetingsDirID).
			WithHeader("Authorization", "Bearer "+link).
			Expect().Status(200))
		obj.Path("$.data.attributes.path").String().IsEqual("/Meetings")
		for _, inc := range obj.Value("included").Array().Iter() {
			if inc.Object().Path("$.attributes.type").String().Raw() == "directory" {
				inc.Object().Path("$.attributes.path").String().IsEqual("/Meetings/Notes")
			}
		}

		byPath := func(p string) *httpexpect.Response {
			return eA.GET(drive+"/metadata").WithQuery("Path", p).
				WithHeader("Authorization", "Bearer "+link).Expect()
		}
		obj = hidden(t, byPath("/Meetings/Notes").Status(200))
		obj.Path("$.data.id").String().IsEqual(notesID)
		returned := obj.Path("$.data.attributes.path").String().Raw()
		assert.Equal(t, "/Meetings/Notes", returned)
		hidden(t, byPath(returned).Status(200)).Path("$.data.id").String().IsEqual(notesID)

		missing := byPath("/Meetings/Missing").Status(404).Body().Raw()
		for _, p := range []string{root + "/Meetings", root, "/Private/Clients" + root + "/Meetings"} {
			assert.Equal(t, missing, byPath(p).Status(404).Body().Raw(), p)
		}
	})
}

func TestSharedDriveRecipientsManageableByTheCaller(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	_, eB, eD := env.createClients(t)
	// Betty can write, Dave can only read
	driveID, rootID, _ := createSharedDrive(t, DriveCreationMethodFromFolder,
		env.acme, env.acmeToken, env.tsA.URL, "Manageable Drive", "dm", nil)
	acceptSharedDrive(t, env.acme, env.betty, "Betty", env.tsA.URL, env.tsB.URL, driveID)
	acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, driveID)

	manageable := func(e *httpexpect.Expect, token, email string) bool {
		data := e.GET("/sharings/drives/"+driveID+"/recipients/"+rootID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Value("data").Array()
		for _, v := range data.Iter() {
			attrs := v.Object().Value("attributes").Object()
			if attrs.Value("email").String().Raw() == email {
				return attrs.Value("sources").Array().Value(0).Object().Value("manageable").Boolean().Raw()
			}
		}
		require.Fail(t, "recipient not found", email)
		return false
	}

	assert.False(t, manageable(eD, env.daveToken, "betty@example.net"))
	assert.True(t, manageable(eB, env.bettyToken, "dave@example.net"))
}
