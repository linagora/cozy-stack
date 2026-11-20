package app_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/stack"
	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestInstallerKonnector(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)

	testutils.NeedCouchdb(t)

	gitURL, done := serveGitRep(t)
	defer done()

	for i := 0; i < 400; i++ {
		if err := exec.Command("git", "ls-remote", gitURL).Run(); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !stackStarted {
		_, _, err := stack.Start()
		if err != nil {
			require.NoError(t, err, "Error while starting job system")
		}
		stackStarted = true
	}

	previousClient := app.ManifestClient
	t.Cleanup(func() { app.ManifestClient = previousClient })
	app.ManifestClient = &http.Client{Transport: &transport{}}

	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, manGen())
	}))
	t.Cleanup(ts.Close)

	db := &instance.Instance{
		ContextName: "foo",
		Prefix:      "app-test",
	}

	require.NoError(t, couchdb.ResetDB(db, consts.Apps))
	require.NoError(t, couchdb.ResetDB(db, consts.Konnectors))
	require.NoError(t, couchdb.ResetDB(db, consts.Files))

	osFS := afero.NewOsFs()
	tmpDir, err := afero.TempDir(osFS, "", "cozy-installer-test")
	if err != nil {
		require.NoError(t, err)
	}
	t.Cleanup(func() { _ = osFS.RemoveAll(tmpDir) })

	baseFS := afero.NewBasePathFs(osFS, tmpDir)
	fs := appfs.NewAferoCopier(baseFS)

	require.NoError(t, couchdb.ResetDB(db, consts.Permissions))

	g, _ := errgroup.WithContext(context.Background())
	couchdb.DefineIndexes(g, db, couchdb.IndexesByDoctype(consts.Files))
	couchdb.DefineIndexes(g, db, couchdb.IndexesByDoctype(consts.Permissions))

	require.NoError(t, g.Wait())

	t.Cleanup(func() {
		assert.NoError(t, couchdb.DeleteDB(db, consts.Apps))
		assert.NoError(t, couchdb.DeleteDB(db, consts.Konnectors))
		assert.NoError(t, couchdb.DeleteDB(db, consts.Files))
		assert.NoError(t, couchdb.DeleteDB(db, consts.Permissions))
	})

	t.Cleanup(func() { assert.NoError(t, localGitCmd.Process.Signal(os.Interrupt)) })

	t.Run("KonnectorInstallSuccessful", func(t *testing.T) {
		manGen = manifestKonnector
		manName = app.KonnectorManifestName

		doUpgrade(t, 1)

		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.KonnectorType,
			Slug:      "local-konnector",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		go inst.Run()

		var state app.State
		var man app.Manifest
		for {
			var done bool
			var err2 error
			man, done, err2 = inst.Poll()
			require.NoError(t, err2)

			if state == "" {
				if !assert.EqualValues(t, app.Installing, man.State()) {
					return
				}
			} else if state == app.Installing {
				if !assert.EqualValues(t, app.Ready, man.State()) {
					return
				}
				require.True(t, done)

				break
			} else {
				t.Fatalf("invalid state")
				return
			}
			state = man.State()
		}

		ok, err := afero.Exists(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"), []byte("1.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")

		inst2, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.KonnectorType,
			Slug:      "local-konnector",
			SourceURL: gitURL,
		})
		assert.Nil(t, inst2)
		assert.Equal(t, app.ErrAlreadyExists, err)
	})

	t.Run("KonnectorUpgradeNotExist", func(t *testing.T) {
		manGen = manifestKonnector
		manName = app.KonnectorManifestName
		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.KonnectorType,
			Slug:      "cozy-konnector-not-exist",
		})
		assert.Nil(t, inst)
		assert.Equal(t, app.ErrNotFound, err)

		inst, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Delete,
			Type:      consts.KonnectorType,
			Slug:      "cozy-konnector-not-exist",
		})
		assert.Nil(t, inst)
		assert.Equal(t, app.ErrNotFound, err)
	})

	t.Run("KonnectorInstallWithUpgrade", func(t *testing.T) {
		manGen = manifestKonnector
		manName = app.KonnectorManifestName

		doUpgrade(t, 1)

		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.KonnectorType,
			Slug:      "cozy-konnector-b",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		go inst.Run()

		var man app.Manifest
		for {
			var done bool
			man, done, err = inst.Poll()
			require.NoError(t, err)

			if done {
				break
			}
		}

		ok, err := afero.Exists(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"), []byte("1.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")

		doUpgrade(t, 2)

		inst, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.KonnectorType,
			Slug:      "cozy-konnector-b",
		})
		require.NoError(t, err)

		go inst.Run()

		var state app.State
		for {
			var done bool
			man, done, err = inst.Poll()
			require.NoError(t, err)

			if state == "" {
				if !assert.EqualValues(t, app.Upgrading, man.State()) {
					return
				}
			} else if state == app.Upgrading {
				if !assert.EqualValues(t, app.Ready, man.State()) {
					return
				}
				require.True(t, done)

				break
			} else {
				t.Fatalf("invalid state")
				return
			}
			state = man.State()
		}

		ok, err = afero.Exists(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"), []byte("2.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")
	})

	t.Run("KonnectorUpdateSkipPerms", func(t *testing.T) {
		// Generating test instance
		finished := true
		conf := config.GetConfig()
		conf.Contexts = map[string]interface{}{
			"foocontext": map[string]interface{}{},
		}

		instance, err := lifecycle.Create(&lifecycle.Options{
			Domain:             "test-skip-perms",
			ContextName:        "foocontext",
			OnboardingFinished: &finished,
		})

		defer func() { _ = lifecycle.Destroy("test-skip-perms") }()

		assert.NoError(t, err)

		manGen = manifestKonnector1
		manName = app.KonnectorManifestName

		inst, err := app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.KonnectorType,
			Slug:      "cozy-konnector-test-skip",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		var man app.Manifest

		man, err = inst.RunSync()
		konnManifest := man.(*app.KonnManifest)
		assert.NoError(t, err)
		assert.Empty(t, konnManifest.AvailableVersion())
		assert.Contains(t, konnManifest.Version(), "1.0.0")

		// Will now update. New perms will be added, preventing an upgrade
		manGen = manifestKonnector2

		inst, err = app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.KonnectorType,
			Slug:      "cozy-konnector-test-skip",
		})
		require.NoError(t, err)

		man, err = inst.RunSync()
		konnManifest = man.(*app.KonnManifest)
		assert.NoError(t, err)
		assert.Contains(t, konnManifest.AvailableVersion(), "2.0.0")
		assert.Contains(t, konnManifest.Version(), "1.0.0") // Assert we stayed on our version

		// Change configuration to tell we skip the verifications
		conf.Contexts = map[string]interface{}{
			"foocontext": map[string]interface{}{
				"permissions_skip_verification": true,
			},
		}

		man2, err := inst.RunSync()
		konnManifest = man2.(*app.KonnManifest)
		assert.NoError(t, err)
		// Assert we upgraded version, and the perms have changed
		assert.False(t, man.Permissions().HasSameRules(man2.Permissions()))
		assert.Empty(t, konnManifest.AvailableVersion())
		assert.Contains(t, konnManifest.Version(), "2.0.0")
	})

	t.Run("KonnectorInstallAndUpgradeWithBranch", func(t *testing.T) {
		manGen = manifestKonnector
		manName = app.KonnectorManifestName
		doUpgrade(t, 3)

		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.KonnectorType,
			Slug:      "local-konnector-branch",
			SourceURL: gitURL + "#branch",
		})
		require.NoError(t, err)

		go inst.Run()

		var state app.State
		var man app.Manifest
		for {
			var done bool
			var err2 error
			man, done, err2 = inst.Poll()
			require.NoError(t, err2)

			if state == "" {
				if !assert.EqualValues(t, app.Installing, man.State()) {
					return
				}
			} else if state == app.Installing {
				if !assert.EqualValues(t, app.Ready, man.State()) {
					return
				}
				require.True(t, done)

				break
			} else {
				t.Fatalf("invalid state")
				return
			}
			state = man.State()
		}

		ok, err := afero.Exists(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"), []byte("3.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")

		doUpgrade(t, 4)

		inst, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.KonnectorType,
			Slug:      "local-konnector-branch",
		})
		require.NoError(t, err)

		go inst.Run()

		state = ""
		for {
			var done bool
			var err2 error
			man, done, err2 = inst.Poll()
			require.NoError(t, err2)

			if state == "" {
				if !assert.EqualValues(t, app.Upgrading, man.State()) {
					return
				}
			} else if state == app.Upgrading {
				if !assert.EqualValues(t, app.Ready, man.State()) {
					return
				}
				require.True(t, done)

				break
			} else {
				t.Fatalf("invalid state")
				return
			}
			state = man.State()
		}

		ok, err = afero.Exists(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.KonnectorManifestName+".br"), []byte("4.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")
	})

	t.Run("KonnectorUninstall", func(t *testing.T) {
		manGen = manifestKonnector
		manName = app.KonnectorManifestName
		inst1, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.KonnectorType,
			Slug:      "konnector-delete",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		go inst1.Run()
		for {
			var done bool
			_, done, err = inst1.Poll()
			require.NoError(t, err)

			if done {
				break
			}
		}
		inst2, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Delete,
			Type:      consts.KonnectorType,
			Slug:      "konnector-delete",
		})
		require.NoError(t, err)

		_, err = inst2.RunSync()
		require.NoError(t, err)

		inst3, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Delete,
			Type:      consts.KonnectorType,
			Slug:      "konnector-delete",
		})
		assert.Nil(t, inst3)
		assert.Equal(t, app.ErrNotFound, err)
	})

	t.Run("KonnectorInstallBadType", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName

		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.KonnectorType,
			Slug:      "cozy-bad-type",
			SourceURL: gitURL,
		})
		assert.NoError(t, err)
		_, err = inst.RunSync()
		assert.Error(t, err)
		assert.ErrorIs(t, err, app.ErrInvalidManifestForKonnector)
	})
}

func compressedFileContainsBytes(fs afero.Fs, filename string, content []byte) (ok bool, err error) {
	f, err := fs.Open(filename)
	if err != nil {
		return
	}
	defer f.Close()
	br := brotli.NewReader(f)
	b, err := io.ReadAll(br)
	if err != nil {
		return
	}
	ok = bytes.Contains(b, content)
	return
}

func manifestKonnector1() string {
	return `{
  "description": "A mini konnector to test cozy-stack-v2",
  "type": "node",
  "developer": {
    "name": "Bruno",
    "url": "cozy.io"
  },
  "license": "MIT",
  "name": "mini-app",
  "permissions": {
	"bills": {
		"type": "io.cozy.bills"
	}
  },
  "slug": "mini",
  "type": "konnector",
  "version": "1.0.0"
}`
}

func manifestKonnector2() string {
	return `{
  "description": "A mini konnector to test cozy-stack-v2",
  "type": "node",
  "developer": {
    "name": "Bruno",
    "url": "cozy.io"
  },
  "license": "MIT",
  "name": "mini-app",
  "permissions": {
	"bills": {
		"type": "io.cozy.bills"
	},
	"files": {
	  "type": "io.cozy.files"
	}
  },
  "slug": "mini",
  "type": "konnector",
  "version": "2.0.0"
}`
}
