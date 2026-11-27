package files

import (
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driveOwner is another instance of the stack, which owns the shared drives
// of which the instance of a recentsEnv is a member.
type driveOwner struct {
	inst   *instance.Instance
	member *instance.Instance
}

// share makes a drive of a folder or a file of the owner, with the other
// instance as a member who has accepted it.
func (o *driveOwner) share(t *testing.T, rootID, rootType string, readOnly bool) *sharing.Sharing {
	t.Helper()
	s := &sharing.Sharing{
		Active:        true,
		Owner:         true,
		Drive:         true,
		DriveRootType: rootType,
		AccessMode:    sharing.AccessModeAdditive,
		Members: []sharing.Member{
			{Status: sharing.MemberStatusOwner, Instance: "http://" + o.inst.Domain},
			{Status: sharing.MemberStatusReady, Instance: "http://" + o.member.Domain, ReadOnly: readOnly},
		},
		Rules:     []sharing.Rule{{Title: "drive", DocType: consts.Files, Values: []string{rootID}}},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, couchdb.CreateDoc(o.inst, s))
	require.NoError(t, s.AddReferenceForSharing(o.inst, &s.Rules[0]))
	return s
}

func (o *driveOwner) mkdir(t *testing.T, fullpath string) *vfs.DirDoc {
	t.Helper()
	dir, err := vfs.MkdirAll(o.inst.VFS(), fullpath)
	require.NoError(t, err)
	return dir
}

// createFile stores an image without content on the instance of the owner,
// and the row of the member for it.
func (o *driveOwner) createFile(t *testing.T, name, dirID, date string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, dirID, 1234, nil, "image/jpeg", "image", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	require.NoError(t, o.inst.VFS().CreateFileDoc(doc))
	o.createRow(t, doc.ID(), o.inst.Domain, date)
	return doc
}

// createRow stores a row of the member for an item of an instance.
func (o *driveOwner) createRow(t *testing.T, id, ownerDomain, date string) {
	t.Helper()
	row := &recents.FileDirActivity{
		DocID:          id,
		OwnerDomain:    ownerDomain,
		LastEditedAt:   date,
		LastActivityAt: date,
		Name:           "last-known-name",
		Type:           consts.FileType,
		SchemaVersion:  recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(o.member, row))
}

// update changes a file of the owner in its document only: a file without
// content cannot be renamed, moved or trashed by the VFS.
func (o *driveOwner) update(t *testing.T, id string, change func(*vfs.FileDoc)) {
	t.Helper()
	doc, err := o.inst.VFS().FileByID(id)
	require.NoError(t, err)
	change(doc)
	require.NoError(t, couchdb.UpdateDoc(o.inst, doc))
}

// countRequests returns the number of requests that fn sends to the databases
// of the member and of the owner. The triggers on the files of the owner run
// jobs in the background. A thumbnail job reads the parent folder of its image
// to open it, so the count starts once these jobs are over. The other jobs
// only write in the databases of the jobs and read the global one.
func (o *driveOwner) countRequests(t *testing.T, fn func()) int {
	t.Helper()
	testutils.WaitForOrFail(t, 10*time.Second, func() bool {
		jobs, err := job.GetQueuedJobs(o.inst, "thumbnail")
		return err == nil && len(jobs) == 0
	})
	member := "/" + o.member.DBPrefix() + "/"
	owner := "/" + o.inst.DBPrefix() + "/"
	return countRequests(t, func(path string) bool {
		if strings.Contains(path, "/io-cozy-jobs") {
			return false
		}
		return strings.HasPrefix(path, member) || strings.HasPrefix(path, owner)
	}, fn)
}

// getRecents returns the whole list, and its raw body.
func (env *recentsEnv) getRecents(t *testing.T) (*httpexpect.Object, string) {
	t.Helper()
	resp := env.e(t).GET("/files/recents").
		WithHeader("Authorization", "Bearer "+env.token).
		Expect().Status(200)
	return resp.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object(), resp.Body().Raw()
}

// docOf returns the document with an ID in the data or in the included
// documents of a page, or nil.
func docOf(page *httpexpect.Object, key, id string) map[string]interface{} {
	docs, _ := page.Raw()[key].([]interface{})
	for _, doc := range docs {
		if doc := doc.(map[string]interface{}); doc["id"] == id {
			return doc
		}
	}
	return nil
}

// fileRelationOf returns the data of the file relationship of a resource. The
// second value is false when the page has no resource with this ID.
func fileRelationOf(page *httpexpect.Object, id string) (interface{}, bool) {
	resource := docOf(page, "data", id)
	if resource == nil {
		return nil, false
	}
	rels := resource["relationships"].(map[string]interface{})
	return rels["file"].(map[string]interface{})["data"], true
}

func attributesOf(doc map[string]interface{}) map[string]interface{} {
	attrs, _ := doc["attributes"].(map[string]interface{})
	return attrs
}

func TestRecentsListCompletesTheDriveItems(t *testing.T) {
	env := recentsSetup(t)
	ownerInst := testutils.NewSetup(t, t.Name()+"Owner").GetTestInstance()
	owner := &driveOwner{inst: ownerInst, member: env.inst}
	fileRef := func(id string) map[string]interface{} {
		return map[string]interface{}{"type": consts.Files, "id": id}
	}

	// The member can read the outer drive and write in the nested one. Nothing
	// of /Private, above the drives, must ever be given.
	team := owner.mkdir(t, "/Private/Team")
	sub := owner.mkdir(t, "/Private/Team/Sub")
	nested := owner.mkdir(t, "/Private/Team/Nested")
	outerDrive := owner.share(t, team.ID(), sharing.DriveRootTypeDirectory, true)
	nestedDrive := owner.share(t, nested.ID(), sharing.DriveRootTypeDirectory, false)

	inSub := owner.createFile(t, "in-sub.jpg", sub.ID(), "2030-01-09T10:00:00Z")
	atRoot := owner.createFile(t, "at-root.jpg", team.ID(), "2030-01-08T10:00:00Z")
	inNested := owner.createFile(t, "in-nested.jpg", nested.ID(), "2030-01-07T10:00:00Z")
	owner.createRow(t, sub.ID(), ownerInst.Domain, "2026-11-09T10:00:00Z")
	owner.createRow(t, team.ID(), ownerInst.Domain, "2026-11-08T10:00:00Z")

	t.Run("an item is given with its drive and its path from the root of the drive", func(t *testing.T) {
		page, body := env.getRecents(t)

		data, ok := fileRelationOf(page, inSub.ID())
		require.True(t, ok)
		assert.Equal(t, fileRef(inSub.ID()), data)
		file := docOf(page, "included", inSub.ID())
		require.NotNil(t, file)
		assert.Equal(t, consts.Files, file["type"])
		attrs := attributesOf(file)
		assert.Equal(t, outerDrive.SID, attrs["driveId"])
		assert.Equal(t, "/Team/Sub/in-sub.jpg", attrs["path"])
		assert.Equal(t, "in-sub.jpg", attrs["name"])
		assert.Equal(t, sub.ID(), attrs["dir_id"])
		assert.Equal(t, false, attrs["trashed"])
		// The links are paths of the files routes, as for an item of the
		// user, and the secret of the thumbnails is one of the instance of the
		// owner.
		links := file["links"].(map[string]interface{})
		assert.Equal(t, "/files/"+inSub.ID(), links["self"])
		secret := strings.TrimSuffix(strings.TrimPrefix(links["small"].(string), "/files/"+inSub.ID()+"/thumbnails/"), "/small")
		got, err := vfs.GetStore().GetThumb(ownerInst, secret)
		require.NoError(t, err)
		assert.Equal(t, inSub.ID(), got)

		folder := docOf(page, "included", sub.ID())
		require.NotNil(t, folder)
		assert.Equal(t, outerDrive.SID, attributesOf(folder)["driveId"])
		assert.Equal(t, "/Team/Sub", attributesOf(folder)["path"])

		// The root of a drive is named by itself.
		root := docOf(page, "included", team.ID())
		require.NotNil(t, root)
		assert.Equal(t, outerDrive.SID, attributesOf(root)["driveId"])
		assert.Equal(t, "/Team", attributesOf(root)["path"])

		assert.NotContains(t, body, "Private")
	})

	t.Run("the drive where the member can write comes first", func(t *testing.T) {
		page, _ := env.getRecents(t)
		file := docOf(page, "included", inNested.ID())
		require.NotNil(t, file)
		assert.Equal(t, nestedDrive.SID, attributesOf(file)["driveId"])
		assert.Equal(t, "/Nested/in-nested.jpg", attributesOf(file)["path"])
	})

	t.Run("the requests do not depend on the number of rows of an owner", func(t *testing.T) {
		count := func(limit string) (requests int, ids []string) {
			requests = owner.countRequests(t, func() {
				ids = idsOf(env.getPage(t, "page[limit]="+limit), "included")
			})
			return requests, ids
		}
		// A first call fills the caches of the instances and of the indexes.
		count("3")
		one, ids := count("1")
		assert.Equal(t, []string{inSub.ID()}, ids)
		three, ids := count("3")
		assert.Equal(t, []string{inSub.ID(), atRoot.ID(), inNested.ID()}, ids)
		assert.Equal(t, one, three)
	})

	t.Run("an item destroyed by its owner is left out", func(t *testing.T) {
		destroyed := owner.createFile(t, "destroyed.jpg", sub.ID(), "2026-11-06T10:00:00Z")
		require.NoError(t, ownerInst.VFS().DeleteFileDoc(destroyed))
		page, _ := env.getRecents(t)
		_, ok := fileRelationOf(page, destroyed.ID())
		assert.False(t, ok)
	})

	t.Run("an item moved out of the drive has no file", func(t *testing.T) {
		moved := owner.createFile(t, "moved.jpg", sub.ID(), "2026-11-05T10:00:00Z")
		outside := owner.mkdir(t, "/Private/Outside")
		owner.update(t, moved.ID(), func(doc *vfs.FileDoc) {
			doc.DirID = outside.ID()
			doc.DocName = "moved-out.jpg"
		})

		page, body := env.getRecents(t)
		data, ok := fileRelationOf(page, moved.ID())
		assert.True(t, ok)
		assert.Nil(t, data)
		assert.Nil(t, docOf(page, "included", moved.ID()))
		assert.NotContains(t, body, "moved-out.jpg")
		assert.NotContains(t, body, "Outside")
	})

	t.Run("a trashed item is given with its path under the trash", func(t *testing.T) {
		trashed := owner.createFile(t, "trashed.jpg", sub.ID(), "2026-11-04T10:00:00Z")
		owner.update(t, trashed.ID(), func(doc *vfs.FileDoc) {
			doc.DirID = consts.TrashDirID
			doc.RestorePath = sub.Fullpath
			doc.Trashed = true
		})
		// A folder trashed with its content, which is restored into a folder
		// that was trashed since: the access is the one on /Private/Team.
		old := owner.mkdir(t, "/Private/Team/Gone/Old")
		child := owner.createFile(t, "child.jpg", old.ID(), "2026-11-03T10:00:00Z")
		owner.createRow(t, old.ID(), ownerInst.Domain, "2026-11-02T10:00:00Z")
		_, err := vfs.TrashDir(ownerInst.VFS(), old)
		require.NoError(t, err)
		gone, err := ownerInst.VFS().DirByPath("/Private/Team/Gone")
		require.NoError(t, err)
		_, err = vfs.TrashDir(ownerInst.VFS(), gone)
		require.NoError(t, err)
		// An item trashed from outside the drives is not given.
		foreign := owner.createFile(t, "foreign.jpg", consts.TrashDirID, "2026-11-01T10:00:00Z")
		owner.update(t, foreign.ID(), func(doc *vfs.FileDoc) {
			doc.RestorePath = "/Private"
			doc.Trashed = true
		})

		page, body := env.getRecents(t)
		file := docOf(page, "included", trashed.ID())
		require.NotNil(t, file)
		data, _ := fileRelationOf(page, trashed.ID())
		assert.Equal(t, fileRef(trashed.ID()), data)
		assert.Equal(t, true, attributesOf(file)["trashed"])
		assert.Equal(t, outerDrive.SID, attributesOf(file)["driveId"])
		assert.Equal(t, "/.cozy_trash/trashed.jpg", attributesOf(file)["path"])

		// A trashed folder is known by its path.
		folder := docOf(page, "included", old.ID())
		require.NotNil(t, folder)
		assert.Equal(t, outerDrive.SID, attributesOf(folder)["driveId"])
		assert.Equal(t, "/.cozy_trash/Old", attributesOf(folder)["path"])
		inFolder := docOf(page, "included", child.ID())
		require.NotNil(t, inFolder)
		assert.Equal(t, outerDrive.SID, attributesOf(inFolder)["driveId"])
		assert.Equal(t, "/.cozy_trash/Old/child.jpg", attributesOf(inFolder)["path"])

		data, ok := fileRelationOf(page, foreign.ID())
		assert.True(t, ok)
		assert.Nil(t, data)
		assert.Nil(t, docOf(page, "included", foreign.ID()))

		// The place where an item was is a path of the owner.
		assert.NotContains(t, body, "restore_path")
		assert.NotContains(t, body, "Private")
	})

	// This test comes last: the member loses both drives.
	t.Run("a revoked member gets only the last name they knew", func(t *testing.T) {
		for _, s := range []*sharing.Sharing{outerDrive, nestedDrive} {
			s.Members[1].Status = sharing.MemberStatusRevoked
			require.NoError(t, couchdb.UpdateDoc(ownerInst, s))
		}
		owner.update(t, inSub.ID(), func(doc *vfs.FileDoc) {
			doc.DocName = "renamed-after-the-revocation.jpg"
		})

		page, body := env.getRecents(t)
		assert.Equal(t, map[string]interface{}{
			"owner_domain":     ownerInst.Domain,
			"last_edited_at":   "2030-01-09T10:00:00Z",
			"last_activity_at": "2030-01-09T10:00:00Z",
			"name":             "last-known-name",
			"type":             "file",
		}, attributesOf(docOf(page, "data", inSub.ID())))
		data, ok := fileRelationOf(page, inSub.ID())
		assert.True(t, ok)
		assert.Nil(t, data)
		page.NotContainsKey("included")
		assert.NotContains(t, body, "renamed-after-the-revocation")
		assert.NotContains(t, body, "/Sub")
	})
}
