package sharings_test

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/require"
)

func TestSharedDrivePublicDownloadPreparationIdentity(t *testing.T) {
	f := setupSharedDrivePermissionFixture(t)
	f.env.acme.FeatureFlags = map[string]interface{}{publiclink.FlagEnabled: true}
	require.NoError(t, instance.Update(f.env.acme))
	folderID := createDirectory(t, f.eOwner, f.productID, "archive-identity", f.ownerAppToken)
	createFile(t, f.eOwner, folderID, "inside.txt", f.ownerAppToken)
	ownerSession, err := session.New(f.env.acme, session.LongRun, "")
	require.NoError(t, err)
	ownerCookie, err := ownerSession.ToCookie()
	require.NoError(t, err)
	recipientSession, err := session.New(f.env.betty, session.LongRun, "")
	require.NoError(t, err)
	recipientCookie, err := recipientSession.ToCookie()
	require.NoError(t, err)

	for _, kind := range []string{"file", "archive"} {
		t.Run(kind, func(t *testing.T) {
			targetID := f.fileID
			if kind == "archive" {
				targetID = folderID
			}
			id, attrs := createSharedDrivePermission(t, f.eOwner, f.sharingID, f.ownerAppToken, "link", "", makeSharedDrivePermissionPayload(t, consts.Files, []string{targetID}, "", nil))
			code := attrs.Value("codes").Object().Value("link").String().Raw()
			var created []publiclink.Event
			require.Eventually(t, func() bool {
				require.NoError(t, couchdb.FindDocsUnoptimized(f.env.acme, consts.PublicLinkEvents, &couchdb.FindRequest{Selector: mango.Equal("link_id", id)}, &created))
				return len(created) == 1
			}, 5*time.Second, 10*time.Millisecond)
			require.Equal(t, publiclink.Created, created[0].Action)
			require.Equal(t, f.env.acme.Domain, created[0].Identity)

			var path string
			if kind == "file" {
				path = f.eOwner.POST("/sharings/drives/"+f.sharingID+"/downloads").
					WithQuery("Id", targetID).WithHeader("Authorization", "Bearer "+code).
					Expect().Status(200).JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
					Object().Path("$.links.related").String().Raw()
			} else {
				path = f.eOwner.POST("/sharings/drives/"+f.sharingID+"/archive").
					WithHeader("Authorization", "Bearer "+code).WithHeader("Content-Type", jsonAPIContentType).
					WithHeader("Cookie", ownerCookie.String()).WithHeader("X-Forwarded-For", "192.0.2.77").
					WithBytes(mustJSON(t, map[string]interface{}{"data": map[string]interface{}{"type": consts.Archives, "attributes": map[string]interface{}{"name": "identity", "ids": []string{targetID}}}})).
					Expect().Status(200).JSON(httpexpect.ContentOpts{MediaType: jsonAPIContentType}).
					Object().Path("$.links.related").String().Raw()
			}
			readDownloads := func() []publiclink.Event {
				var events []publiclink.Event
				require.NoError(t, couchdb.FindDocsUnoptimized(f.env.acme, consts.PublicLinkEvents, &couchdb.FindRequest{Selector: mango.And(mango.Equal("link_id", id), mango.Equal("action", string(publiclink.Downloaded)))}, &events))
				return events
			}
			require.Eventually(t, func() bool { return len(readDownloads()) == 1 }, 5*time.Second, 10*time.Millisecond)
			if kind == "file" {
				require.Empty(t, readDownloads()[0].Identity, "the anonymous preparer owns the intent, not subsequent visitors")
			} else {
				require.Equal(t, f.env.acme.Domain, readDownloads()[0].Identity)
				require.Equal(t, "192.0.2.77", readDownloads()[0].IP)
			}

			for _, visitor := range []string{"anonymous-owner", "anonymous-http-proxy", "anonymous-same-stack", "recipient-session-same-stack", "owner-token", "owner-session"} {
				t.Run(visitor, func(t *testing.T) {
					base := f.env.tsA.URL
					if visitor == "anonymous-http-proxy" || visitor == "anonymous-same-stack" || visitor == "recipient-session-same-stack" {
						base = f.env.tsB.URL
						recipient, err := sharing.FindSharing(f.env.betty, f.sharingID)
						require.NoError(t, err)
						if visitor == "anonymous-http-proxy" {
							ownerURL, err := url.Parse(recipient.Members[0].Instance)
							require.NoError(t, err)
							_, err = lifecycle.GetInstance(ownerURL.Host)
							require.Error(t, err, "exercise the actual HTTP proxy, not the shortcut")
						} else {
							oldURL := recipient.Members[0].Instance
							recipient.Members[0].Instance = "http://" + f.env.acme.Domain
							require.NoError(t, couchdb.UpdateDoc(f.env.betty, recipient))
							t.Cleanup(func() {
								recipient.Members[0].Instance = oldURL
								require.NoError(t, couchdb.UpdateDoc(f.env.betty, recipient))
							})
						}
					}

					before := readDownloads()
					req, err := http.NewRequest(http.MethodGet, base+path+"?Dl=1", nil)
					require.NoError(t, err)
					if visitor == "owner-token" {
						req.Header.Set("Authorization", "Bearer "+f.ownerAppToken)
					} else if visitor == "owner-session" {
						req.AddCookie(ownerCookie)
					} else if visitor == "recipient-session-same-stack" {
						req.AddCookie(recipientCookie)
					} else {
						require.Empty(t, req.Header.Get("Authorization"))
						require.Empty(t, req.Header.Get("Cookie"))
					}
					res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
					require.NoError(t, err)
					defer res.Body.Close()
					body, err := io.ReadAll(res.Body)
					require.NoError(t, err)
					require.Equal(t, 200, res.StatusCode)
					if kind == "file" {
						require.Equal(t, "foo", string(body))
					} else {
						archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
						require.NoError(t, err)
						require.Len(t, archive.File, 2)
						var contents []string
						for _, entry := range archive.File {
							if entry.FileInfo().IsDir() {
								continue
							}
							r, err := entry.Open()
							require.NoError(t, err)
							data, err := io.ReadAll(r)
							require.NoError(t, err)
							require.NoError(t, r.Close())
							contents = append(contents, string(data))
						}
						require.Equal(t, []string{"foo"}, contents)
					}

					require.Equal(t, before, readDownloads(), "secret consumption must not create an event or change its identity")
				})
			}
			if kind == "file" {
				f.eOwner.GET("/sharings/drives/"+f.sharingID+"/download/"+targetID).
					WithQuery("Dl", "1").WithHeader("Authorization", "Bearer "+code).
					Expect().Status(200).Body().IsEqual("foo")
				require.Eventually(t, func() bool { return len(readDownloads()) == 2 }, 5*time.Second, 10*time.Millisecond,
					"direct shared-drive downloads must still count")
			}
		})
	}
	var recipientEvents []publiclink.Event
	require.NoError(t, couchdb.FindDocsUnoptimized(f.env.betty, consts.PublicLinkEvents, &couchdb.FindRequest{Selector: mango.Exists("link_id")}, &recipientEvents))
	require.Empty(t, recipientEvents)
}
