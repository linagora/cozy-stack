package office

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/office"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/metrics"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const forceSavedContent = "saved by the forcesave"

// fakeCommandServer plays the document server: it answers the forcesave
// command with the configured error code and, on success, calls the stack
// back with the saved document after its answer, as OnlyOffice does.
type fakeCommandServer struct {
	callbackURL  string
	result       atomic.Int32
	commands     atomic.Int32
	noCallback   atomic.Bool
	commandDelay atomic.Int64
}

func (f *fakeCommandServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/saved" {
		_, _ = w.Write([]byte(forceSavedContent))
		return
	}
	var cmd struct {
		C   string `json:"c"`
		Key string `json:"key"`
	}
	if r.URL.Path != "/coauthoring/CommandService.ashx" || json.NewDecoder(r.Body).Decode(&cmd) != nil || cmd.C != "forcesave" {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	f.commands.Add(1)
	time.Sleep(time.Duration(f.commandDelay.Load()))
	result := f.result.Load()
	_, _ = fmt.Fprintf(w, `{"key":%q,"error":%d}`, cmd.Key, result)
	if result != 0 || f.noCallback.Load() {
		return
	}
	savedURL := "http://" + r.Host + "/saved"
	go func() {
		time.Sleep(200 * time.Millisecond)
		body := fmt.Sprintf(`{"key":%q,"status":6,"url":%q}`, cmd.Key, savedURL)
		res, err := http.Post(f.callbackURL, "application/json", bytes.NewBufferString(body))
		if err == nil {
			res.Body.Close()
		}
	}()
}

func TestDownloadDuringAnEditingSession(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	oo := &fakeCommandServer{}
	ooServer := httptest.NewServer(oo)
	t.Cleanup(ooServer.Close)
	config.GetConfig().Office = map[string]config.Office{
		"default": {OnlyOfficeURL: ooServer.URL + "/"},
	}
	timeout := office.ForceSaveTimeout
	office.ForceSaveTimeout = 2 * time.Second
	t.Cleanup(func() { office.ForceSaveTimeout = timeout })
	// Each case asks OnlyOffice again, except the one about the debounce.
	office.SetForceSaveDebounce(0)
	t.Cleanup(func() { office.SetForceSaveDebounce(office.ForceSaveDebounce) })

	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient(consts.Files)
	fileID := createFile(t, inst)

	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/office": Routes,
		"/files":  files.Routes,
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	oo.callbackURL = ts.URL + "/office/callback"

	e := testutils.CreateTestClient(t, ts.URL)
	download := func() string {
		return e.GET("/files/download/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Body().Raw()
	}

	t.Run("WithoutSession", func(t *testing.T) {
		assert.Equal(t, "", download())
		assert.Equal(t, int32(0), oo.commands.Load())
	})

	document := e.GET("/office/"+fileID+"/open").
		WithHeader("Authorization", "Bearer "+token).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object().Path("$.data.attributes.onlyoffice.document").Object()
	docServerURL, err := url.Parse(document.Value("url").String().Raw())
	require.NoError(t, err)

	t.Run("NothingToSave", func(t *testing.T) {
		oo.result.Store(4)
		assert.Equal(t, "", download())
		assert.Equal(t, int32(1), oo.commands.Load())
	})

	t.Run("EditedInTheSession", func(t *testing.T) {
		oo.result.Store(0)
		assert.Equal(t, forceSavedContent, download())
		assert.Equal(t, int32(2), oo.commands.Load())
	})

	t.Run("ConcurrentReaders", func(t *testing.T) {
		oo.commandDelay.Store(int64(300 * time.Millisecond))
		defer oo.commandDelay.Store(0)
		const readers = 5
		bodies := make(chan string, readers)
		var wg sync.WaitGroup
		for range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodGet, ts.URL+"/files/download/"+fileID, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					bodies <- err.Error()
					return
				}
				defer res.Body.Close()
				body, _ := io.ReadAll(res.Body)
				bodies <- string(body)
			}()
		}
		wg.Wait()
		close(bodies)
		for body := range bodies {
			assert.Equal(t, forceSavedContent, body)
		}
		assert.Equal(t, int32(3), oo.commands.Load())
	})

	t.Run("SlowCommandAndNoCallback", func(t *testing.T) {
		oo.noCallback.Store(true)
		oo.commandDelay.Store(int64(1500 * time.Millisecond))
		defer oo.noCallback.Store(false)
		defer oo.commandDelay.Store(0)
		timeouts := metrics.OfficeForceSaveUnconfirmed.WithLabelValues("timeout")
		before := promtestutil.ToFloat64(timeouts)
		start := time.Now()
		assert.Equal(t, forceSavedContent, download())
		assert.Less(t, time.Since(start), office.ForceSaveTimeout+500*time.Millisecond)
		assert.Equal(t, int32(4), oo.commands.Load())
		assert.Equal(t, before+1, promtestutil.ToFloat64(timeouts))
	})

	t.Run("HeadRequest", func(t *testing.T) {
		e.HEAD("/files/download/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
		assert.Equal(t, int32(4), oo.commands.Load())
	})

	t.Run("DownloadByTheDocumentServer", func(t *testing.T) {
		assert.Equal(t, "1", docServerURL.Query().Get(office.DocumentServerParam))
		assert.Equal(t, forceSavedContent, e.GET(docServerURL.Path).
			WithQueryString(docServerURL.RawQuery).
			Expect().Status(200).
			Body().Raw())
		assert.Equal(t, int32(4), oo.commands.Load())
	})

	t.Run("LaterChunkOfARangeRead", func(t *testing.T) {
		e.GET("/files/download/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Range", "bytes=5-10").
			Expect().Status(http.StatusPartialContent)
		assert.Equal(t, int32(4), oo.commands.Load())
	})

	t.Run("NotAnOfficeDocument", func(t *testing.T) {
		image, err := vfs.NewFileDoc("photo.jpg", consts.RootDirID, -1, nil,
			"image/jpeg", "image", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err := inst.VFS().CreateFile(image, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		_, err = office.GetStore().AddDoc(inst, office.ConflictDetector{ID: image.ID(), Rev: image.Rev(), MD5Sum: image.MD5Sum})
		require.NoError(t, err)

		e.GET("/files/download/"+image.ID()).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
		assert.Equal(t, int32(4), oo.commands.Load())
	})

	t.Run("SequentialReadersWithinTheDebounce", func(t *testing.T) {
		const delay = time.Second
		office.SetForceSaveDebounce(delay)
		defer office.SetForceSaveDebounce(0)
		// Leave the delay of the commands of the cases above
		time.Sleep(delay)
		for range 4 {
			assert.Equal(t, forceSavedContent, download())
		}
		assert.Equal(t, int32(5), oo.commands.Load())

		time.Sleep(delay)
		assert.Equal(t, forceSavedContent, download())
		assert.Equal(t, int32(6), oo.commands.Load())
	})

	t.Run("UnsavedAnswersWithinTheDebounce", func(t *testing.T) {
		const delay = time.Second
		office.SetForceSaveDebounce(delay)
		defer office.SetForceSaveDebounce(0)
		defer oo.result.Store(0)
		commands := oo.commands.Load()
		// 4: nothing new to save; 1: no session for the key, as after an
		// opening in view mode that no editor has loaded
		for _, result := range []int32{4, 1} {
			time.Sleep(delay)
			oo.result.Store(result)
			for range 4 {
				download()
			}
			commands++
			assert.Equal(t, commands, oo.commands.Load())
		}
		// The last outcome holds for the delay it was recorded with.
		time.Sleep(delay)
	})

	t.Run("ConflictCopy", func(t *testing.T) {
		doc, err := vfs.NewFileDoc("conflict.docx", consts.RootDirID, -1, nil,
			"application/msword", "text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err := inst.VFS().CreateFile(doc, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		e.GET("/office/"+doc.ID()+"/open").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
		key, err := office.GetStore().GetSecretByID(inst, doc.ID())
		require.NoError(t, err)

		// Another client changes the file during the session
		old, err := inst.VFS().FileByID(doc.ID())
		require.NoError(t, err)
		other := old.Clone().(*vfs.FileDoc)
		other.ByteSize = -1
		other.MD5Sum = nil
		f, err = inst.VFS().CreateFile(other, old)
		require.NoError(t, err)
		_, err = io.WriteString(f, "uploaded by another client")
		require.NoError(t, err)
		require.NoError(t, f.Close())

		saves := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(r.URL.Path[1:]))
		}))
		defer saves.Close()
		for _, content := range []string{"first-save", "second-save"} {
			e.POST("/office/callback").
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(fmt.Sprintf(`{"key":%q,"status":6,"url":%q}`, key, saves.URL+"/"+content))).
				Expect().Status(200)
		}

		copied, err := inst.VFS().FileByPath("/conflict (2).docx")
		require.NoError(t, err)
		assert.Equal(t, "second-save", string(readFile(t, inst, copied.ID())))
		_, err = inst.VFS().FileByPath("/conflict (3).docx")
		assert.Error(t, err)
		assert.Equal(t, "uploaded by another client", string(readFile(t, inst, doc.ID())))

		commands := oo.commands.Load()
		e.GET("/files/download/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
		assert.Equal(t, commands, oo.commands.Load())
	})

	t.Run("SessionClosedJustBefore", func(t *testing.T) {
		doc, err := vfs.NewFileDoc("closed.docx", consts.RootDirID, -1, nil,
			"application/msword", "text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err := inst.VFS().CreateFile(doc, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		read, err := inst.VFS().FileByID(doc.ID())
		require.NoError(t, err)

		// The final save writes the file and removes the session, while a
		// reader has already read the file and found the key of the session.
		saved := read.Clone().(*vfs.FileDoc)
		saved.ByteSize = -1
		saved.MD5Sum = nil
		f, err = inst.VFS().CreateFile(saved, read)
		require.NoError(t, err)
		_, err = io.WriteString(f, "final save")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		require.NoError(t, office.GetStore().UpdateSecret(inst, "closed-session", "", doc.ID()))
		current, err := inst.VFS().FileByID(doc.ID())
		require.NoError(t, err)
		require.NotEqual(t, read.Rev(), current.Rev())

		commands := oo.commands.Load()
		served := office.SaveOpenSession(context.Background(), inst, read)
		assert.Equal(t, current.Rev(), served.Rev())
		assert.Equal(t, commands, oo.commands.Load())
	})

	t.Run("ReaderLeaves", func(t *testing.T) {
		oo.commandDelay.Store(int64(500 * time.Millisecond))
		defer oo.commandDelay.Store(0)
		unconfirmed := func() float64 {
			sum := 0.0
			for _, reason := range []string{"store", "timeout", "error", "reread"} {
				sum += promtestutil.ToFloat64(metrics.OfficeForceSaveUnconfirmed.WithLabelValues(reason))
			}
			return sum
		}
		before := unconfirmed()
		doc, err := inst.VFS().FileByID(fileID)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()

		assert.Equal(t, doc.Rev(), office.SaveOpenSession(ctx, inst, doc).Rev())
		assert.Equal(t, before, unconfirmed())

		// Another reader joins the forcesave, which goes on without the first.
		saved := office.SaveOpenSession(context.Background(), inst, doc)
		assert.NotEqual(t, doc.Rev(), saved.Rev())
		assert.Equal(t, int32(9), oo.commands.Load())
	})

	t.Run("WrittenWhileWaiting", func(t *testing.T) {
		oo.noCallback.Store(true)
		defer oo.noCallback.Store(false)
		old, err := inst.VFS().FileByID(fileID)
		require.NoError(t, err)
		go func() {
			time.Sleep(300 * time.Millisecond)
			newdoc := old.Clone().(*vfs.FileDoc)
			newdoc.ByteSize = -1
			newdoc.MD5Sum = nil
			f, err := inst.VFS().CreateFile(newdoc, old)
			if err != nil {
				return
			}
			_, _ = io.WriteString(f, "written while waiting")
			_ = f.Close()
		}()

		res := e.GET("/files/download/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
		body := res.Body().Raw()
		sum := md5.Sum([]byte(body))
		assert.Equal(t, "written while waiting", body)
		assert.Equal(t, `"`+base64.StdEncoding.EncodeToString(sum[:])+`"`, res.Header("Etag").Raw())
		assert.Equal(t, int32(10), oo.commands.Load())
		// The forcesave of this reader ends at its own bound, just after it
		time.Sleep(500 * time.Millisecond)
	})
}
