package sharings_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const driveUploadPartSize = 6 << 20

func TestSharedDriveResumableUploads(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	backends := []struct {
		name      string
		configure func(t *testing.T)
	}{
		// The test configuration keeps afero in memory: the name of the drive
		// is too long for a file system on disk.
		{"afero", func(t *testing.T) {}},
		{"s3", func(t *testing.T) {
			fixture := testutils.StartMinio(t)
			fs := config.Fs{
				URL: fixture.FsURL(),
				S3:  config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "drive-upload"}}},
			}
			require.NoError(t, config.InitS3Connection(fs))
			config.GetConfig().Fs.URL = fs.URL
			config.GetConfig().Fs.S3 = fs.S3
		}},
	}

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			env := setupSharedDrivesEnvWithConfig(t, nil, func() {
				b.configure(t)
				config.GetConfig().Fs.ResumableUploads = config.FsResumableUploads{
					PartSize:       driveUploadPartSize,
					MaxAge:         time.Hour,
					MaxPerAccount:  20,
					MaxPerInstance: 200,
				}
			})
			prefix := "/sharings/drives/" + env.firstSharingID
			betty := &driveUploadClient{t: t, base: env.tsB.URL, token: env.bettyToken}

			t.Run("RelayedCutThenResumed", func(t *testing.T) {
				cutThenResumed(t, env, betty, prefix, "relayed.bin")
			})

			t.Run("SameStackCutThenResumed", func(t *testing.T) {
				ownerOnTheSameStack(t, env)
				cutThenResumed(t, env, betty, prefix, "same-stack.bin")
			})

			t.Run("ContentReplacedInSteps", func(t *testing.T) {
				content := randomUploadBytes(driveUploadPartSize+10, 2)
				res := betty.create(http.MethodPut, prefix+"/"+env.checklistID, nil, false)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				location := res.Header.Get("Location")
				assertDriveUploadLocation(t, prefix, location)

				done := betty.patch(location, 0, content, true)
				require.Equal(t, http.StatusOK, done.StatusCode, string(done.body))
				assert.Equal(t, "?1", done.Header.Get("Upload-Complete"))
				id, attrs := done.fileAttrs(t)
				assert.Equal(t, env.checklistID, id)
				assert.Equal(t, env.firstSharingID, attrs["driveId"])
				assert.Equal(t, strconv.Itoa(len(content)), attrs["size"])
				assertOwnerContent(t, env, id, content)
			})

			t.Run("CancelledThenGone", func(t *testing.T) {
				res := betty.create(http.MethodPost, prefix+"/"+env.meetingsDirID+"?Type=file&Name=gone.bin", nil, false)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				location := res.Header.Get("Location")

				del := betty.do(http.MethodDelete, location, nil, 0, nil)
				assert.Equal(t, http.StatusNoContent, del.StatusCode, string(del.body))
				assert.Equal(t, http.StatusNotFound, betty.head(location).StatusCode)
				gone := betty.patch(location, 0, []byte("late"), true)
				assert.Equal(t, http.StatusNotFound, gone.StatusCode)
				gone.problem(t)
			})

			t.Run("UploadOutsideTheDrive", func(t *testing.T) {
				acme := &driveUploadClient{t: t, base: env.tsA.URL, token: env.acmeToken}
				res := acme.create(http.MethodPost, "/files/"+env.outsideOfShareID+"?Type=file&Name=private.bin", nil, false)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				location := prefix + "/uploads/" + path.Base(res.Header.Get("Location"))

				assert.Equal(t, http.StatusNotFound, betty.head(location).StatusCode)
				assert.Equal(t, http.StatusNotFound, betty.patch(location, 0, []byte("x"), true).StatusCode)
				assert.Equal(t, http.StatusNotFound, betty.do(http.MethodDelete, location, nil, 0, nil).StatusCode)
				assert.Equal(t, http.StatusNoContent, acme.head(res.Header.Get("Location")).StatusCode)
			})

			t.Run("MemberStackNotUpdated", func(t *testing.T) {
				// A stack without resumable uploads relays the request as it
				// is: the owner sees the drive token without the relay header.
				bettySharing, err := sharing.FindSharing(env.betty, env.firstSharingID)
				require.NoError(t, err)
				old := &driveUploadClient{t: t, base: env.tsA.URL, token: bettySharing.Credentials[0].DriveToken}
				target := prefix + "/" + env.meetingsDirID + "?Type=file&Name=old-stack.bin"

				// Without the content, the request is refused rather than
				// creating an empty file.
				empty := old.create(http.MethodPost, target, nil, false)
				assert.Equal(t, http.StatusNotImplemented, empty.StatusCode, string(empty.body))
				empty.problem(t)
				_, err = env.acme.VFS().FileByPath("/" + env.rootDirName + "/Meetings/old-stack.bin")
				assert.Error(t, err)

				content := randomUploadBytes(driveUploadPartSize+10, 4)
				res := old.create(http.MethodPost, target, content, true)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				assert.Empty(t, res.interim)
				assert.Empty(t, res.Header.Get("Location"))
				id, _ := res.fileAttrs(t)
				assertOwnerContent(t, env, id, content)
			})

			t.Run("NoteWithoutContent", func(t *testing.T) {
				res := betty.create(http.MethodPost, prefix+"/"+env.meetingsDirID+"?Type=file&Name=empty.cozy-note", nil, false)
				assert.Equal(t, http.StatusNotImplemented, res.StatusCode, string(res.body))
				res.problem(t)
				_, err := env.acme.VFS().FileByPath("/" + env.rootDirName + "/Meetings/empty.cozy-note")
				assert.Error(t, err)
			})

			t.Run("OwnerWithoutResumableUploads", func(t *testing.T) {
				owner := newFakeOwner(t, env.tsA.URL, "old")
				ownerBehind(t, env, owner.URL())
				target := prefix + "/" + env.meetingsDirID + "?Type=file&Name=old-owner.bin"

				res := betty.create(http.MethodPost, target, nil, false)
				assert.Equal(t, http.StatusNotImplemented, res.StatusCode, string(res.body))
				res.problem(t)
				_, err := env.acme.VFS().FileByPath("/" + env.rootDirName + "/Meetings/old-owner.bin")
				assert.Error(t, err)

				before, err := env.acme.VFS().FileByID(env.checklistID)
				require.NoError(t, err)
				res = betty.create(http.MethodPut, prefix+"/"+env.checklistID, nil, false)
				assert.Equal(t, http.StatusNotImplemented, res.StatusCode, string(res.body))
				after, err := env.acme.VFS().FileByID(env.checklistID)
				require.NoError(t, err)
				assert.Equal(t, before.Rev(), after.Rev())

				options := betty.do(http.MethodOptions, prefix+"/uploads", nil, 0, nil)
				assert.Equal(t, http.StatusNoContent, options.StatusCode)
				assert.Empty(t, options.Header.Get("Upload-Limit"))
				assert.Equal(t, int32(1), owner.probes.Load(), "the answer of the owner is kept")
			})

			t.Run("OwnerWithResumableUploads", func(t *testing.T) {
				owner := newFakeOwner(t, env.tsA.URL, "recent")
				ownerBehind(t, env, owner.URL())
				for i := range 2 {
					res := betty.create(http.MethodPost, prefix+"/"+env.meetingsDirID+"?Type=file&Name=recent-owner-"+strconv.Itoa(i)+".bin", nil, false)
					require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
					assertDriveUploadLocation(t, prefix, res.Header.Get("Location"))
				}
				options := betty.do(http.MethodOptions, prefix+"/uploads", nil, 0, nil)
				assert.Equal(t, "min-size=0", options.Header.Get("Upload-Limit"))
				assert.Equal(t, int32(1), owner.probes.Load(), "the answer of the owner is kept")
			})

			t.Run("OptionsWithoutTheRightOnFiles", func(t *testing.T) {
				owner := newFakeOwner(t, env.tsA.URL, "recent")
				ownerBehind(t, env, owner.URL())
				contacts := &driveUploadClient{t: t, base: env.tsB.URL, token: generateAppToken(env.betty, "contacts", consts.Contacts)}
				res := contacts.do(http.MethodOptions, prefix+"/uploads", nil, 0, nil)
				assert.Equal(t, http.StatusForbidden, res.StatusCode)
				assert.Equal(t, int32(0), owner.probes.Load(), "the owner is not asked")
			})

			t.Run("OwnerRedirects", func(t *testing.T) {
				// The stack of the member does not follow a redirection with
				// the drive token, and does not keep this answer.
				owner := newFakeOwner(t, env.tsA.URL, "redirect")
				ownerBehind(t, env, owner.URL())
				for i := range 2 {
					res := betty.create(http.MethodPost, prefix+"/"+env.meetingsDirID+"?Type=file&Name=redirected-"+strconv.Itoa(i)+".bin", nil, false)
					assert.Equal(t, http.StatusNotImplemented, res.StatusCode, string(res.body))
				}
				assert.Equal(t, int32(2), owner.probes.Load())
			})

			t.Run("OwnerUnreachable", func(t *testing.T) {
				owner := newFakeOwner(t, env.tsA.URL, "down")
				ownerBehind(t, env, owner.URL())
				target := prefix + "/" + env.meetingsDirID + "?Type=file&Name=unreachable.bin"
				res := betty.create(http.MethodPost, target, nil, false)
				assert.Equal(t, http.StatusBadGateway, res.StatusCode, string(res.body))

				// The failure is not kept: the next upload asks again.
				owner.mode.Store("recent")
				res = betty.create(http.MethodPost, target, nil, false)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				assertDriveUploadLocation(t, prefix, res.Header.Get("Location"))
			})

			t.Run("UploadOfAnotherMember", func(t *testing.T) {
				sid, rootID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
					"Two writers", "Drive with two writers", []RecipientInfo{
						{Name: "Betty", Email: "betty@example.net"},
						{Name: "Dave", Email: "dave@example.net"},
					})
				acceptSharedDrive(t, env.acme, env.betty, "Betty", env.tsA.URL, env.tsB.URL, sid)
				acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, sid)
				dave := &driveUploadClient{t: t, base: env.tsD.URL, token: env.daveToken}

				res := betty.create(http.MethodPost, "/sharings/drives/"+sid+"/"+rootID+"?Type=file&Name=betty.bin", nil, false)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				location := res.Header.Get("Location")

				// Betty is a member of both drives: the upload stays under its own.
				other := prefix + "/uploads/" + path.Base(location)
				assert.Equal(t, http.StatusNotFound, betty.head(other).StatusCode)
				assert.Equal(t, http.StatusNotFound, betty.patch(other, 0, []byte("x"), false).StatusCode)

				assert.Equal(t, http.StatusNotFound, dave.head(location).StatusCode)
				assert.Equal(t, http.StatusNotFound, dave.patch(location, 0, []byte("dave"), false).StatusCode)
				head := betty.head(location)
				require.Equal(t, http.StatusNoContent, head.StatusCode)
				assert.Equal(t, "0", head.Header.Get("Upload-Offset"))

				assert.Equal(t, http.StatusNotFound, dave.do(http.MethodDelete, location, nil, 0, nil).StatusCode)
				assert.Equal(t, http.StatusNoContent, betty.head(location).StatusCode)
			})

			t.Run("TooManyUploadsRelayed", func(t *testing.T) {
				sid, rootID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
					"Limits", "Drive for the limits", []RecipientInfo{
						{Name: "Betty", Email: "betty@example.net"},
						{Name: "Dave", Email: "dave@example.net"},
					})
				acceptSharedDrive(t, env.acme, env.betty, "Betty", env.tsA.URL, env.tsB.URL, sid)
				acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, sid)
				dave := &driveUploadClient{t: t, base: env.tsD.URL, token: env.daveToken}
				acme := &driveUploadClient{t: t, base: env.tsA.URL, token: env.acmeToken}
				n := 0
				create := func(c *driveUploadClient, target string) *driveUploadResponse {
					n++
					return c.create(http.MethodPost, target+"?Type=file&Name=limit-"+strconv.Itoa(n)+".bin", nil, false)
				}
				inDrive := "/sharings/drives/" + sid + "/" + rootID
				inOwnDrive := "/files/" + env.outsideOfShareID

				cfg := &config.GetConfig().Fs.ResumableUploads
				old := *cfg
				t.Cleanup(func() { *cfg = old })
				_, byAccount := ownerActiveUploads(t, env)
				cfg.MaxPerAccount = max(byAccount["betty@example.net"], byAccount["dave@example.net"], byAccount[""]) + 1
				cfg.MaxPerInstance = 1000

				for i := byAccount["betty@example.net"]; i < cfg.MaxPerAccount; i++ {
					require.Equal(t, http.StatusCreated, create(betty, inDrive).StatusCode)
				}
				refused := create(betty, inDrive)
				assert.Equal(t, http.StatusTooManyRequests, refused.StatusCode)
				refused.problem(t)
				assert.Equal(t, http.StatusCreated, create(dave, inDrive).StatusCode)
				assert.Equal(t, http.StatusCreated, create(acme, inOwnDrive).StatusCode)

				// The instance is full: nobody can start another upload.
				cfg.MaxPerInstance, _ = ownerActiveUploads(t, env)
				assert.Equal(t, http.StatusTooManyRequests, create(dave, inDrive).StatusCode)
				assert.Equal(t, http.StatusTooManyRequests, create(acme, inOwnDrive).StatusCode)
			})

			t.Run("ReadOnlyDuringUpload", func(t *testing.T) {
				content := randomUploadBytes(driveUploadPartSize+10, 3)
				res := betty.create(http.MethodPost, prefix+"/"+env.meetingsDirID+"?Type=file&Name=read-only.bin", nil, false)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				location := res.Header.Get("Location")

				setBettyReadOnly(t, env, true)
				assert.Equal(t, http.StatusForbidden, betty.head(location).StatusCode)
				assert.Equal(t, http.StatusForbidden, betty.patch(location, 0, content, true).StatusCode)
				assert.Equal(t, http.StatusForbidden, betty.do(http.MethodDelete, location, nil, 0, nil).StatusCode)

				setBettyReadOnly(t, env, false)
				done := betty.patch(location, 0, content, true)
				require.Equal(t, http.StatusCreated, done.StatusCode, string(done.body))
			})

			t.Run("RemovedDuringUpload", func(t *testing.T) {
				res := betty.create(http.MethodPost, prefix+"/"+env.meetingsDirID+"?Type=file&Name=removed.bin", nil, false)
				require.Equal(t, http.StatusCreated, res.StatusCode, string(res.body))
				location := res.Header.Get("Location")

				bettySharing, err := sharing.FindSharing(env.betty, env.firstSharingID)
				require.NoError(t, err)
				driveToken := bettySharing.Credentials[0].DriveToken
				index := findSharingMemberIndexByEmail(t, env.acme, env.firstSharingID, "betty@example.net")
				newSharingExpect(t, env.tsA.URL).
					DELETE("/sharings/"+env.firstSharingID+"/recipients/"+strconv.Itoa(index)).
					WithHeader("Authorization", "Bearer "+env.acmeToken).
					Expect().Status(http.StatusNoContent)

				assert.Equal(t, http.StatusForbidden, betty.head(location).StatusCode)
				assert.Equal(t, http.StatusForbidden, betty.patch(location, 0, []byte("late"), true).StatusCode)
				// The owner refuses the drive token of Betty, whatever her stack does.
				direct := &driveUploadClient{t: t, base: env.tsA.URL, token: driveToken}
				assert.Equal(t, http.StatusForbidden, direct.head(location).StatusCode)
				assert.Equal(t, http.StatusForbidden, direct.patch(location, 0, []byte("late"), true).StatusCode)
				assert.Equal(t, int64(0), ownerUpload(t, env, location).Offset)
			})
		})
	}
}

func cutThenResumed(t *testing.T, env *sharedDrivesEnv, c *driveUploadClient, prefix, name string) {
	content := randomUploadBytes(2*driveUploadPartSize+4321, 1)
	cut := int64(driveUploadPartSize + 1234)
	announced := make(chan struct{})
	res, err := c.send(http.MethodPost, prefix+"/"+env.meetingsDirID+"?Type=file&Name="+name,
		&cutUploadBody{bytes.NewReader(content), cut, announced}, int64(len(content)), map[string]string{
			"Upload-Draft-Interop-Version": "6",
			"Upload-Complete":              "?1",
			"Content-MD5":                  md5Base64Of(content),
			"Content-Type":                 "application/octet-stream",
		}, announced)
	require.Error(t, err)
	location := res.interim
	assertDriveUploadLocation(t, prefix, location)
	require.Eventually(t, func() bool {
		return ownerUpload(t, env, location).Offset == cut
	}, 30*time.Second, 50*time.Millisecond)

	head := c.head(location)
	require.Equal(t, http.StatusNoContent, head.StatusCode, string(head.body))
	assert.Equal(t, strconv.FormatInt(cut, 10), head.Header.Get("Upload-Offset"))
	assert.Equal(t, "?0", head.Header.Get("Upload-Complete"))

	done := c.patch(location, cut, content[cut:], true)
	require.Equal(t, http.StatusCreated, done.StatusCode, string(done.body))
	assert.Equal(t, "?1", done.Header.Get("Upload-Complete"))
	id, attrs := done.fileAttrs(t)
	assert.Equal(t, name, attrs["name"])
	assert.Equal(t, env.firstSharingID, attrs["driveId"])
	assert.Equal(t, md5Base64Of(content), attrs["md5sum"])
	assertOwnerContent(t, env, id, content)

	// The final response is lost: the same request gets it again.
	again := c.patch(location, cut, content[cut:], true)
	require.Equal(t, http.StatusCreated, again.StatusCode, string(again.body))
	againID, _ := again.fileAttrs(t)
	assert.Equal(t, id, againID)
}

// ownerOnTheSameStack makes Betty's stack serve the drive requests directly,
// instead of relaying them over HTTP.
func ownerOnTheSameStack(t *testing.T, env *sharedDrivesEnv) {
	s, err := sharing.FindSharing(env.betty, env.firstSharingID)
	require.NoError(t, err)
	ownerURL := s.Members[0].Instance
	s.Members[0].Instance = "http://" + env.acme.Domain
	require.NoError(t, couchdb.UpdateDoc(env.betty, s))
	t.Cleanup(func() {
		s, err := sharing.FindSharing(env.betty, env.firstSharingID)
		require.NoError(t, err)
		s.Members[0].Instance = ownerURL
		require.NoError(t, couchdb.UpdateDoc(env.betty, s))
	})
}

// fakeOwner stands in front of the stack of the owner: "recent" relays every
// request, "old" refuses OPTIONS like a stack without resumable uploads, and
// "down" drops the connections.
type fakeOwner struct {
	srv    *httptest.Server
	mode   atomic.Value
	probes atomic.Int32
}

func newFakeOwner(t *testing.T, ownerURL, mode string) *fakeOwner {
	target, err := url.Parse(ownerURL)
	require.NoError(t, err)
	relay := httputil.NewSingleHostReverseProxy(target)
	f := &fakeOwner{}
	f.mode.Store(mode)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := f.mode.Load().(string)
		if mode == "down" {
			panic(http.ErrAbortHandler)
		}
		if r.Method == http.MethodOptions {
			f.probes.Add(1)
			switch mode {
			case "old":
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			case "redirect":
				http.Redirect(w, r, ownerURL+r.URL.Path, http.StatusTemporaryRedirect)
				return
			}
		}
		r.Host = target.Host
		relay.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOwner) URL() string { return f.srv.URL }

// ownerBehind makes Betty's stack reach the owner at the given URL.
func ownerBehind(t *testing.T, env *sharedDrivesEnv, ownerURL string) {
	s, err := sharing.FindSharing(env.betty, env.firstSharingID)
	require.NoError(t, err)
	previous := s.Members[0].Instance
	s.Members[0].Instance = ownerURL
	require.NoError(t, couchdb.UpdateDoc(env.betty, s))
	t.Cleanup(func() {
		s, err := sharing.FindSharing(env.betty, env.firstSharingID)
		require.NoError(t, err)
		s.Members[0].Instance = previous
		require.NoError(t, couchdb.UpdateDoc(env.betty, s))
	})
}

func setBettyReadOnly(t *testing.T, env *sharedDrivesEnv, readOnly bool) {
	index := findSharingMemberIndexByEmail(t, env.acme, env.firstSharingID, "betty@example.net")
	e := newSharingExpect(t, env.tsA.URL)
	target := "/sharings/" + env.firstSharingID + "/recipients/" + strconv.Itoa(index) + "/readonly"
	if readOnly {
		e.POST(target).WithHeader("Authorization", "Bearer "+env.acmeToken).Expect().Status(http.StatusNoContent)
	} else {
		e.DELETE(target).WithHeader("Authorization", "Bearer "+env.acmeToken).Expect().Status(http.StatusNoContent)
	}
	waitForSharingMemberReadOnly(t, env.acme, env.firstSharingID, "betty@example.net", readOnly)
}

func assertDriveUploadLocation(t *testing.T, prefix, location string) {
	t.Helper()
	require.NotEmpty(t, location)
	assert.Equal(t, prefix+"/uploads", path.Dir(location))
}

func ownerUpload(t *testing.T, env *sharedDrivesEnv, location string) *vfs.Upload {
	u := &vfs.Upload{}
	require.NoError(t, couchdb.GetDoc(env.acme, consts.FilesUploads, path.Base(location), u))
	return u
}

// ownerActiveUploads counts the uploads in progress on the owner's instance,
// in all and by drive member (empty for the owner).
func ownerActiveUploads(t *testing.T, env *sharedDrivesEnv) (int, map[string]int) {
	var uploads []*vfs.Upload
	err := couchdb.GetAllDocs(env.acme, consts.FilesUploads, &couchdb.AllDocsRequest{Limit: 1000}, &uploads)
	if !couchdb.IsNoDatabaseError(err) {
		require.NoError(t, err)
	}
	total, byAccount := 0, map[string]int{}
	for _, u := range uploads {
		if !u.Completed {
			total++
			byAccount[u.DriveMember]++
		}
	}
	return total, byAccount
}

func assertOwnerContent(t *testing.T, env *sharedDrivesEnv, id string, content []byte) {
	t.Helper()
	fs := env.acme.VFS()
	doc, err := fs.FileByID(id)
	require.NoError(t, err)
	f, err := fs.OpenFile(doc)
	require.NoError(t, err)
	got, err := io.ReadAll(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.True(t, bytes.Equal(content, got), "content of %s", doc.DocName)
}

// driveUploadClient speaks the resumable upload protocol, like URLSession.
type driveUploadClient struct {
	t     *testing.T
	base  string
	token string
}

type driveUploadResponse struct {
	*http.Response
	body []byte
	// interim is the Location announced by a 104 interim response.
	interim string
}

// send closes announced, when given, at the 104 interim response.
func (c *driveUploadClient) send(method, target string, body io.Reader, size int64, headers map[string]string, announced chan struct{}) (*driveUploadResponse, error) {
	res := &driveUploadResponse{}
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			if code == 104 && res.interim == "" {
				res.interim = header.Get("Location")
				if announced != nil {
					close(announced)
				}
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), method, c.base+target, body)
	require.NoError(c.t, err)
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+c.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	res.Response = resp
	res.body, err = io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return res, nil
}

func (c *driveUploadClient) do(method, target string, body []byte, size int64, headers map[string]string) *driveUploadResponse {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	res, err := c.send(method, target, r, size, headers, nil)
	require.NoError(c.t, err)
	return res
}

// create starts an upload, with the whole content or without any.
func (c *driveUploadClient) create(method, target string, content []byte, complete bool) *driveUploadResponse {
	return c.do(method, target, content, int64(len(content)), map[string]string{
		"Upload-Draft-Interop-Version": "6",
		"Upload-Complete":              sfBool(complete),
		"Content-Type":                 "application/octet-stream",
	})
}

func (c *driveUploadClient) head(location string) *driveUploadResponse {
	return c.do(http.MethodHead, location, nil, 0, map[string]string{"Upload-Draft-Interop-Version": "6"})
}

func (c *driveUploadClient) patch(location string, offset int64, content []byte, complete bool) *driveUploadResponse {
	return c.do(http.MethodPatch, location, content, int64(len(content)), map[string]string{
		"Upload-Draft-Interop-Version": "6",
		"Upload-Complete":              sfBool(complete),
		"Upload-Offset":                strconv.FormatInt(offset, 10),
		"Content-Type":                 "application/partial-upload",
	})
}

func (r *driveUploadResponse) fileAttrs(t *testing.T) (string, map[string]interface{}) {
	var doc struct {
		Data struct {
			ID         string                 `json:"id"`
			Attributes map[string]interface{} `json:"attributes"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.body, &doc), string(r.body))
	return doc.Data.ID, doc.Data.Attributes
}

func (r *driveUploadResponse) problem(t *testing.T) {
	assert.Equal(t, "application/problem+json", r.Header.Get("Content-Type"), string(r.body))
}

// cutUploadBody loses the connection after n bytes, once the upload has been
// announced: the 104 can take longer than the bytes through the relay.
type cutUploadBody struct {
	r         io.Reader
	n         int64
	announced <-chan struct{}
}

func (c *cutUploadBody) Read(p []byte) (int, error) {
	if c.n <= 0 {
		select {
		case <-c.announced:
		case <-time.After(10 * time.Second):
		}
		return 0, errors.New("connection lost")
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

func sfBool(b bool) string {
	if b {
		return "?1"
	}
	return "?0"
}

func randomUploadBytes(size int, seed int64) []byte {
	buf := make([]byte, size)
	_, _ = rand.New(rand.NewSource(seed)).Read(buf)
	return buf
}

func md5Base64Of(content []byte) string {
	sum := md5.Sum(content)
	return base64.StdEncoding.EncodeToString(sum[:])
}
