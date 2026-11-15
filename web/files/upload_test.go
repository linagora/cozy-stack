package files

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResumableUploadsHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	require.NoError(t, loadLocale(), "Could not load default locale translations")
	testutils.NeedCouchdb(t)
	config.GetConfig().Fs.ResumableUploads = config.FsResumableUploads{
		PartSize:       uploadTestPartSize,
		MaxAge:         time.Hour,
		MaxPerInstance: 20,
	}

	for _, b := range uploadWebBackends() {
		t.Run(b.name, func(t *testing.T) {
			b.configure(t)
			setup := testutils.NewSetup(t, "upload-"+b.name)
			inst := setup.GetTestInstance(&lifecycle.Options{SwiftLayout: 2})
			_, token := setup.GetTestClient(consts.Files)
			ts := newUploadServer(t, inst)
			c := &uploadClient{t: t, base: ts.URL, token: token}

			t.Run("CutThenResumed", func(t *testing.T) {
				content := randomBytes(2*uploadTestPartSize+4321, 1)
				cut := int64(uploadTestPartSize + 1234)
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=cut.bin",
					&cutBody{bytes.NewReader(content), cut}, int64(len(content)), map[string]string{
						"Upload-Draft-Interop-Version": "6",
						"Upload-Complete":              "?1",
						"Content-MD5":                  md5Base64(content),
						"Content-Type":                 "application/octet-stream",
					})
				require.Error(t, err)
				require.NotEmpty(t, res.interim, "the 104 response announces the upload")
				location := res.interim
				waitUploadOffset(t, inst, uploadIDOf(location), cut)

				head := c.head(location)
				assert.Equal(t, http.StatusNoContent, head.StatusCode)
				assert.Equal(t, strconv.FormatInt(cut, 10), head.Header.Get("Upload-Offset"))
				assert.Equal(t, "?0", head.Header.Get("Upload-Complete"))
				assert.Equal(t, "no-store", head.Header.Get("Cache-Control"))
				assert.Contains(t, head.Header.Get("Upload-Limit"), "expires=")

				done := c.patch(location, cut, content[cut:], true)
				require.Equal(t, http.StatusCreated, done.StatusCode, string(done.body))
				assert.Equal(t, "?1", done.Header.Get("Upload-Complete"))
				id, attrs := done.fileAttrs(t)
				assert.Equal(t, "cut.bin", attrs["name"])
				assert.Equal(t, strconv.Itoa(len(content)), attrs["size"])
				assert.Equal(t, md5Base64(content), attrs["md5sum"])

				doc, err := inst.VFS().FileByID(id)
				require.NoError(t, err)
				f, err := inst.VFS().OpenFile(doc)
				require.NoError(t, err)
				got, err := io.ReadAll(f)
				require.NoError(t, err)
				require.NoError(t, f.Close())
				assert.True(t, bytes.Equal(content, got))

				dl, err := c.do(http.MethodGet, "/files/download/"+id, nil, 0, map[string]string{
					"Range": "bytes=" + strconv.Itoa(uploadTestPartSize-10) + "-" + strconv.Itoa(uploadTestPartSize+9),
				})
				require.NoError(t, err)
				assert.Equal(t, http.StatusPartialContent, dl.StatusCode)
				assert.Equal(t, content[uploadTestPartSize-10:uploadTestPartSize+10], dl.body)

				// The final response was lost: HEAD says the upload is complete,
				// and PATCH replays the response without a second file.
				head = c.head(location)
				assert.Equal(t, "?1", head.Header.Get("Upload-Complete"))
				assert.Equal(t, strconv.Itoa(len(content)), head.Header.Get("Upload-Offset"))
				again := c.patch(location, int64(len(content)), nil, true)
				require.Equal(t, http.StatusCreated, again.StatusCode, string(again.body))
				againID, _ := again.fileAttrs(t)
				assert.Equal(t, id, againID)
				again = c.patch(location, cut, content[cut:], true)
				require.Equal(t, http.StatusCreated, again.StatusCode, string(again.body))
				againID, _ = again.fileAttrs(t)
				assert.Equal(t, id, againID)
			})

			t.Run("CreatedFirstThenAppended", func(t *testing.T) {
				content := randomBytes(uploadTestPartSize+999, 2)
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=web.bin", nil, 0, map[string]string{
					"Upload-Draft-Interop-Version": "10",
					"Upload-Complete":              "?0",
					"Upload-Length":                strconv.Itoa(len(content)),
					"Origin":                       "https://drive.example.net",
				})
				require.NoError(t, err)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				location := res.Header.Get("Location")
				require.NotEmpty(t, location)
				assert.Equal(t, location, res.interim)
				assert.Equal(t, "0", res.Header.Get("Upload-Offset"))
				assert.Equal(t, "?0", res.Header.Get("Upload-Complete"))
				assert.Contains(t, res.Header.Get("Upload-Limit"), "max-age=")
				assert.Contains(t, res.Header.Get("Access-Control-Expose-Headers"), "Upload-Offset")

				mid := 3 << 20
				part := c.patch(location, 0, content[:mid], false)
				require.Equal(t, http.StatusNoContent, part.StatusCode, string(part.body))
				assert.Equal(t, strconv.Itoa(mid), part.Header.Get("Upload-Offset"))

				// A wrong offset is refused with the expected one.
				wrong := c.patch(location, 42, content[42:], true)
				assert.Equal(t, http.StatusConflict, wrong.StatusCode)
				assert.Equal(t, strconv.Itoa(mid), wrong.Header.Get("Upload-Offset"))
				p := wrong.problem(t)
				assert.Equal(t, "https://iana.org/assignments/http-problem-types#mismatching-upload-offset", p["type"])
				assert.EqualValues(t, mid, p["expected-offset"])
				assert.EqualValues(t, 42, p["provided-offset"])

				done := c.patch(location, int64(mid), content[mid:], true)
				require.Equal(t, http.StatusCreated, done.StatusCode, string(done.body))
				_, attrs := done.fileAttrs(t)
				assert.Equal(t, md5Base64(content), attrs["md5sum"])
			})

			t.Run("ResumedAfterRestart", func(t *testing.T) {
				content := randomBytes(uploadTestPartSize+77, 3)
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=restart.bin", nil, 0, map[string]string{
					"Upload-Draft-Interop-Version": "6",
					"Upload-Complete":              "?0",
				})
				require.NoError(t, err)
				location := res.Header.Get("Location")
				part := c.patch(location, 0, content[:1000], false)
				require.Equal(t, http.StatusNoContent, part.StatusCode)

				// A new stack, with a new instance and VFS loaded from CouchDB.
				fresh, err := lifecycle.GetInstance(inst.Domain)
				require.NoError(t, err)
				other := &uploadClient{t: t, base: newUploadServer(t, fresh).URL, token: token}
				head := other.head(location)
				assert.Equal(t, "1000", head.Header.Get("Upload-Offset"))
				done := other.patch(location, 1000, content[1000:], true)
				require.Equal(t, http.StatusCreated, done.StatusCode, string(done.body))
				_, attrs := done.fileAttrs(t)
				assert.Equal(t, md5Base64(content), attrs["md5sum"])
			})

			t.Run("Cancelled", func(t *testing.T) {
				content := randomBytes(uploadTestPartSize+10, 4)
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=cancel.bin", bytes.NewReader(content), int64(len(content)), map[string]string{
					"Upload-Draft-Interop-Version": "6",
					"Upload-Complete":              "?0",
				})
				require.NoError(t, err)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				location := res.Header.Get("Location")
				assert.NotEmpty(t, b.leftovers(t, inst, uploadIDOf(location)))

				del, err := c.do(http.MethodDelete, location, nil, 0, nil)
				require.NoError(t, err)
				assert.Equal(t, http.StatusNoContent, del.StatusCode)
				assert.Empty(t, b.leftovers(t, inst, uploadIDOf(location)))

				assert.Equal(t, http.StatusNotFound, c.head(location).StatusCode)
				patch := c.patch(location, int64(len(content)), []byte("more"), true)
				assert.Equal(t, http.StatusNotFound, patch.StatusCode)
				assert.Equal(t, "Upload not found", patch.problem(t)["title"])
				assert.Empty(t, b.leftovers(t, inst, uploadIDOf(location)))
				exists, err := inst.VFS().DirChildExists(consts.RootDirID, "cancel.bin")
				require.NoError(t, err)
				assert.False(t, exists)
			})

			t.Run("ExpiredAndCleaned", func(t *testing.T) {
				content := randomBytes(uploadTestPartSize+10, 5)
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=abandoned.bin", bytes.NewReader(content), int64(len(content)), map[string]string{
					"Upload-Draft-Interop-Version": "6",
					"Upload-Complete":              "?0",
				})
				require.NoError(t, err)
				location := res.Header.Get("Location")
				id := uploadIDOf(location)
				assert.True(t, job.System().HasTrigger(inst, job.TriggerInfos{Type: "@cron", WorkerType: "clean-uploads"}))

				expireWebUpload(t, inst, id)
				assert.Equal(t, http.StatusNotFound, c.head(location).StatusCode)
				removed, err := vfs.CleanUploads(inst.VFS())
				require.NoError(t, err)
				assert.Equal(t, 1, removed)
				assert.Empty(t, b.leftovers(t, inst, id))

				patch := c.patch(location, int64(len(content)), []byte("late"), true)
				assert.Equal(t, http.StatusNotFound, patch.StatusCode)
				assert.Equal(t, "Upload not found", patch.problem(t)["title"])
				assert.Empty(t, b.leftovers(t, inst, id))
			})

			t.Run("CleanedDuringPatch", func(t *testing.T) {
				content := randomBytes(2*uploadTestPartSize, 6)
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=race.bin", nil, 0, map[string]string{
					"Upload-Draft-Interop-Version": "6",
					"Upload-Complete":              "?0",
				})
				require.NoError(t, err)
				location := res.Header.Get("Location")
				id := uploadIDOf(location)

				pr, pw := io.Pipe()
				done := make(chan *uploadResponse, 1)
				go func() {
					res, err := c.do(http.MethodPatch, location, pr, int64(len(content)), map[string]string{
						"Upload-Draft-Interop-Version": "6",
						"Upload-Complete":              "?1",
						"Upload-Offset":                "0",
						"Content-Type":                 "application/partial-upload",
					})
					if err != nil {
						res = nil
					}
					done <- res
				}()
				_, err = pw.Write(content[:uploadTestPartSize+10])
				require.NoError(t, err)
				waitUploadOffset(t, inst, id, uploadTestPartSize)

				expireWebUpload(t, inst, id)
				removed, err := vfs.CleanUploads(inst.VFS())
				require.NoError(t, err)
				assert.Equal(t, 1, removed)

				_, _ = pw.Write(content[uploadTestPartSize+10:])
				_ = pw.Close()
				patch := <-done
				require.NotNil(t, patch)
				assert.Equal(t, http.StatusNotFound, patch.StatusCode)
				assert.Empty(t, b.leftovers(t, inst, id))
				exists, err := inst.VFS().DirChildExists(consts.RootDirID, "race.bin")
				require.NoError(t, err)
				assert.False(t, exists)
			})

			t.Run("WrongMD5", func(t *testing.T) {
				content := randomBytes(uploadTestPartSize+10, 7)
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=md5.bin", nil, 0, map[string]string{
					"Upload-Draft-Interop-Version": "6",
					"Upload-Complete":              "?0",
					"Content-MD5":                  md5Base64([]byte("other")),
				})
				require.NoError(t, err)
				location := res.Header.Get("Location")
				done := c.patch(location, 0, content, true)
				assert.Equal(t, http.StatusPreconditionFailed, done.StatusCode)
				assert.Equal(t, "?1", done.Header.Get("Upload-Complete"))
				assert.Equal(t, http.StatusNotFound, c.head(location).StatusCode)
				assert.Empty(t, b.leftovers(t, inst, uploadIDOf(location)))
			})

			t.Run("InvalidRequestOnACompletedUpload", func(t *testing.T) {
				content := randomBytes(uploadTestPartSize+10, 10)
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=replayed.bin", nil, 0, map[string]string{
					"Upload-Draft-Interop-Version": "6",
					"Upload-Complete":              "?0",
				})
				require.NoError(t, err)
				location := res.Header.Get("Location")
				done := c.patch(location, 0, content, true)
				require.Equal(t, http.StatusCreated, done.StatusCode, string(done.body))

				// Only the request that completes the upload is replayed.
				wrong := c.patch(location, 10, content[10:20], true)
				assert.Equal(t, http.StatusConflict, wrong.StatusCode)
				assert.Equal(t, "?0", wrong.Header.Get("Upload-Complete"))
				notLast := c.patch(location, 0, content, false)
				assert.Equal(t, http.StatusConflict, notLast.StatusCode)
				replay := c.patch(location, 0, content, true)
				assert.Equal(t, http.StatusCreated, replay.StatusCode)
			})

			t.Run("ResumedElsewhereIsNotComplete", func(t *testing.T) {
				// A POST with the whole file is resumed by a HEAD, as URLSession
				// does after a lost connection: the POST must not say that the
				// upload is complete, or the client would not resume it.
				content := randomBytes(2*uploadTestPartSize, 9)
				poster := &uploadClient{t: t, base: ts.URL, token: token, announced: make(chan string, 1)}
				pr, pw := io.Pipe()
				done := make(chan *uploadResponse, 1)
				go func() {
					res, _ := poster.do(http.MethodPost, "/files/?Type=file&Name=elsewhere.bin", pr, int64(len(content)), map[string]string{
						"Upload-Draft-Interop-Version": "6",
						"Upload-Complete":              "?1",
					})
					done <- res
				}()
				go func() {
					_, _ = pw.Write(content[:uploadTestPartSize+10])
				}()
				location := <-poster.announced
				waitUploadOffset(t, inst, uploadIDOf(location), uploadTestPartSize)

				head := c.head(location)
				assert.Equal(t, strconv.Itoa(uploadTestPartSize), head.Header.Get("Upload-Offset"))
				go func() {
					_, _ = pw.Write(content[uploadTestPartSize+10:])
					_ = pw.Close()
				}()
				res := <-done
				require.NotNil(t, res.Response)
				assert.Equal(t, http.StatusConflict, res.StatusCode)
				assert.Equal(t, "?0", res.Header.Get("Upload-Complete"))

				// The client resumes, and finishes the upload.
				head = c.head(location)
				offset, err := strconv.Atoi(head.Header.Get("Upload-Offset"))
				require.NoError(t, err)
				done2 := c.patch(location, int64(offset), content[offset:], true)
				require.Equal(t, http.StatusCreated, done2.StatusCode, string(done2.body))
			})

			t.Run("UnknownUpload", func(t *testing.T) {
				assert.Equal(t, http.StatusNotFound, c.head("/files/uploads/unknown").StatusCode)
				patch := c.patch("/files/uploads/unknown", 0, []byte("data"), true)
				assert.Equal(t, http.StatusNotFound, patch.StatusCode)
				assert.Equal(t, "Upload not found", patch.problem(t)["title"])
				del, err := c.do(http.MethodDelete, "/files/uploads/unknown", nil, 0, nil)
				require.NoError(t, err)
				assert.Equal(t, http.StatusNotFound, del.StatusCode)
			})

			t.Run("Overwrite", func(t *testing.T) {
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=overwrite.bin", bytes.NewReader([]byte("v1")), 2, nil)
				require.NoError(t, err)
				require.Equal(t, http.StatusCreated, res.StatusCode)
				id, _ := res.fileAttrs(t)

				content := randomBytes(uploadTestPartSize+10, 8)
				res, err = c.do(http.MethodPut, "/files/"+id, bytes.NewReader(content), int64(len(content)), map[string]string{
					"Upload-Draft-Interop-Version": "6",
					"Upload-Complete":              "?1",
					"Content-MD5":                  md5Base64(content),
				})
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, res.StatusCode, string(res.body))
				assert.NotEmpty(t, res.interim)
				newID, attrs := res.fileAttrs(t)
				assert.Equal(t, id, newID)
				assert.Equal(t, md5Base64(content), attrs["md5sum"])
			})

			t.Run("SmallFileTakesTheUsualWay", func(t *testing.T) {
				content := []byte("a small photo")
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=small.txt", bytes.NewReader(content), int64(len(content)), map[string]string{
					"Upload-Draft-Interop-Version": "6",
					"Upload-Complete":              "?1",
				})
				require.NoError(t, err)
				require.Equal(t, http.StatusCreated, res.StatusCode)
				assert.Empty(t, res.interim)
			})

			t.Run("OtherPermissions", func(t *testing.T) {
				res, err := c.do(http.MethodPost, "/files/?Type=file&Name=perm.bin", nil, 0, map[string]string{
					"Upload-Draft-Interop-Version": "6",
					"Upload-Complete":              "?0",
				})
				require.NoError(t, err)
				location := res.Header.Get("Location")
				_, other := setup.GetTestClient(consts.Contacts)
				stranger := &uploadClient{t: t, base: ts.URL, token: other}
				assert.Equal(t, http.StatusForbidden, stranger.head(location).StatusCode)
				assert.Equal(t, http.StatusForbidden, stranger.patch(location, 0, []byte("x"), true).StatusCode)
				head := c.head(location)
				assert.Equal(t, "0", head.Header.Get("Upload-Offset"))
			})
		})
	}
}

// TestResumableUploadsDatabase checks that a new instance gets the
// io.cozy.files.uploads database only with its first resumable upload.
func TestResumableUploadsDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	require.NoError(t, loadLocale(), "Could not load default locale translations")
	testutils.NeedCouchdb(t)
	config.GetConfig().Fs.URL = &url.URL{Scheme: "file", Host: "localhost", Path: t.TempDir()}
	setup := testutils.NewSetup(t, "upload-database")
	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient(consts.Files)
	c := &uploadClient{t: t, base: newUploadServer(t, inst).URL, token: token}
	exists := func() bool {
		_, err := couchdb.DBStatus(inst, consts.FilesUploads)
		if couchdb.IsNoDatabaseError(err) {
			return false
		}
		require.NoError(t, err)
		return true
	}
	require.False(t, exists(), "a new instance has no database for the uploads")

	assert.Equal(t, http.StatusNotFound, c.head("/files/uploads/unknown").StatusCode)
	assert.Equal(t, http.StatusNotFound, c.patch("/files/uploads/unknown", 0, []byte("x"), true).StatusCode)
	del, err := c.do(http.MethodDelete, "/files/uploads/unknown", nil, 0, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, del.StatusCode)
	_, err = vfs.CleanUploads(inst.VFS())
	require.NoError(t, err)
	assert.False(t, exists())

	res, err := c.do(http.MethodPost, "/files/?Type=file&Name=first.bin", nil, 0, map[string]string{
		"Upload-Draft-Interop-Version": "6",
		"Upload-Complete":              "?0",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
	assert.True(t, exists())
}

// dropLink returns a client with the token of a sharing by link that can add
// files to dirID, like a public drop folder.
func dropLink(t *testing.T, inst *instance.Instance, base, dirID string) *uploadClient {
	token, err := inst.MakeJWT(consts.ShareAudience, "drop-"+dirID, consts.Files, "", time.Now())
	require.NoError(t, err)
	rules := permission.Set{{
		Type:   consts.Files,
		Verbs:  permission.Verbs(permission.GET, permission.POST),
		Values: []string{dirID},
	}}
	expires := time.Now().Add(time.Hour)
	_, err = permission.CreateShareSet(inst, &permission.Permission{Type: "app", Permissions: rules}, "",
		map[string]string{"drop-" + dirID: token}, nil, permission.Permission{Permissions: rules}, &expires, false)
	require.NoError(t, err)
	return &uploadClient{t: t, base: base, token: token}
}

func (c *uploadClient) mkdir(name string) string {
	res, err := c.do(http.MethodPost, "/files/?Type=directory&Name="+url.QueryEscape(name), nil, 0, nil)
	require.NoError(c.t, err)
	require.Equal(c.t, http.StatusCreated, res.StatusCode, string(res.body))
	id, _ := res.fileAttrs(c.t)
	return id
}

// TestResumableUploadsByLink checks the uploads made with the token of a
// sharing by link.
func TestResumableUploadsByLink(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	require.NoError(t, loadLocale(), "Could not load default locale translations")
	testutils.NeedCouchdb(t)
	config.GetConfig().Fs.ResumableUploads = config.FsResumableUploads{
		PartSize:       uploadTestPartSize,
		MaxAge:         time.Hour,
		MaxPerInstance: 20,
	}
	config.GetConfig().Fs.URL = &url.URL{Scheme: "file", Host: "localhost", Path: t.TempDir()}
	setup := testutils.NewSetup(t, "upload-by-link")
	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient(consts.Files)
	ts := newUploadServer(t, inst)
	owner := &uploadClient{t: t, base: ts.URL, token: token}

	t.Run("CompletedUploadOfAMovedFile", func(t *testing.T) {
		drop := owner.mkdir("drop")
		private := owner.mkdir("private")
		link := dropLink(t, inst, ts.URL, drop)

		res, err := link.do(http.MethodPost, "/files/"+drop+"?Type=file&Name=from-link.bin", nil, 0, map[string]string{
			"Upload-Draft-Interop-Version": "6",
			"Upload-Complete":              "?0",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
		location := res.Header.Get("Location")
		content := randomBytes(uploadTestPartSize+10, 20)
		done := link.patch(location, 0, content, true)
		require.Equal(t, http.StatusCreated, done.StatusCode, string(done.body))
		id, _ := done.fileAttrs(t)

		// The owner moves the file out of the drop folder.
		body := `{"data":{"type":"io.cozy.files","id":"` + id + `","attributes":{"dir_id":"` + private + `","name":"report.bin"}}}`
		moved, err := owner.do(http.MethodPatch, "/files/"+id, bytes.NewReader([]byte(body)), int64(len(body)),
			map[string]string{"Content-Type": "application/vnd.api+json"})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, moved.StatusCode, string(moved.body))

		// The link has no access to the file anymore, nor to its upload.
		head := link.head(location)
		assert.Equal(t, http.StatusNotFound, head.StatusCode)
		replay := link.patch(location, int64(len(content)), nil, true)
		assert.Equal(t, http.StatusNotFound, replay.StatusCode)
		assert.Empty(t, replay.body)
		// The owner still has.
		assert.Equal(t, http.StatusNoContent, owner.head(location).StatusCode)
	})

	t.Run("NoSpaceAnnouncedToALink", func(t *testing.T) {
		inst.BytesDiskQuota = 200 << 20
		defer func() { inst.BytesDiskQuota = 0 }()
		drop := owner.mkdir("drop-limit")
		link := dropLink(t, inst, ts.URL, drop)
		start := func(c *uploadClient, name string) *uploadResponse {
			res, err := c.do(http.MethodPost, "/files/"+drop+"?Type=file&Name="+name, nil, 0, map[string]string{
				"Upload-Draft-Interop-Version": "10",
				"Upload-Complete":              "?0",
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
			return res
		}

		// The space left on the instance is not for the visitors of a link.
		res := start(link, "by-link.bin")
		assert.NotContains(t, res.Header.Get("Upload-Limit"), "max-size")
		assert.Contains(t, res.Header.Get("Upload-Limit"), "max-age=")
		assert.NotContains(t, link.head(res.Header.Get("Location")).Header.Get("Upload-Limit"), "max-size")

		res = start(owner, "by-owner.bin")
		assert.Contains(t, res.Header.Get("Upload-Limit"), "max-size=")
	})
}

// TestResumableUploadNameTaken checks that a name already taken is refused
// when the upload is created, like the usual upload, before any byte is
// stored.
func TestResumableUploadNameTaken(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	require.NoError(t, loadLocale(), "Could not load default locale translations")
	testutils.NeedCouchdb(t)
	config.GetConfig().Fs.ResumableUploads = config.FsResumableUploads{
		PartSize:       uploadTestPartSize,
		MaxAge:         time.Hour,
		MaxPerInstance: 20,
	}
	config.GetConfig().Fs.URL = &url.URL{Scheme: "file", Host: "localhost", Path: t.TempDir()}
	setup := testutils.NewSetup(t, "upload-name-taken")
	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient(consts.Files)
	c := &uploadClient{t: t, base: newUploadServer(t, inst).URL, token: token}

	res, err := c.do(http.MethodPost, "/files/?Type=file&Name=taken.bin", bytes.NewReader([]byte("v1")), 2, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	res, err = c.do(http.MethodPost, "/files/?Type=file&Name=taken.bin", nil, 0, map[string]string{
		"Upload-Draft-Interop-Version": "6",
		"Upload-Complete":              "?0",
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, res.StatusCode)
	assert.Empty(t, res.interim)

	content := randomBytes(2*uploadTestPartSize, 30)
	read := &countingReader{r: bytes.NewReader(content)}
	res, err = c.do(http.MethodPost, "/files/?Type=file&Name=taken.bin", read, int64(len(content)), map[string]string{
		"Upload-Draft-Interop-Version": "6",
		"Upload-Complete":              "?1",
	})
	if err == nil {
		assert.Equal(t, http.StatusConflict, res.StatusCode)
		assert.Empty(t, res.interim)
	}
	assert.Less(t, read.n, int64(len(content)), "the content must not be read")
	_, err = couchdb.DBStatus(inst, consts.FilesUploads)
	assert.True(t, couchdb.IsNoDatabaseError(err), "no upload must have been created")
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
