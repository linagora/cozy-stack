package app_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
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

func TestInstallerWebApp(t *testing.T) {
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

	t.Run("WebappInstallBadSlug", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName
		_, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			SourceURL: "git://foo.bar",
		})
		if assert.Error(t, err) {
			assert.Equal(t, app.ErrInvalidSlugName, err)
		}

		_, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "coucou/",
			SourceURL: "git://foo.bar",
		})
		if assert.Error(t, err) {
			assert.Equal(t, app.ErrInvalidSlugName, err)
		}
	})

	t.Run("WebappInstallBadAppsSource", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName
		_, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "app3",
			SourceURL: "foo://bar.baz",
		})
		if assert.Error(t, err) {
			assert.Equal(t, app.ErrNotSupportedSource, err)
		}

		_, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "app4",
			SourceURL: "git://bar  .baz",
		})
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "invalid character")
		}

		_, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "app5",
			SourceURL: "",
		})
		if assert.Error(t, err) {
			assert.Equal(t, app.ErrMissingSource, err)
		}
	})

	t.Run("WebappInstallSuccessful", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName

		doUpgrade(t, 1)

		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "local-cozy-mini",
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

		ok, err := afero.Exists(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"), []byte("1.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")

		inst2, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "local-cozy-mini",
			SourceURL: gitURL,
		})
		assert.Nil(t, inst2)
		assert.Equal(t, app.ErrAlreadyExists, err)
	})

	t.Run("WebappInstallSuccessfulWithExtraPerms", func(t *testing.T) {
		manifest1 := func() string {
			return `{
"description": "A mini app to test cozy-stack-v2",
"developer": {
  "name": "Cozy",
  "url": "cozy.io"
},
"license": "MIT",
"name": "mini-app",
"permissions": {
  "rule0": {
    "type": "io.cozy.files",
    "verbs": ["GET"],
    "values": ["foobar"]
  },
  "rule1": {
    "type": "cc.cozycloud.sentry",
    "verbs": ["POST"]
  }
},
"slug": "mini-test-perms",
"type": "webapp",
"version": "1.0.0"
}`
		}

		manifest2 := func() string {
			return `{
"description": "A mini app to test cozy-stack-v2",
"developer": {
  "name": "Cozy",
  "url": "cozy.io"
},
"license": "MIT",
"name": "mini-app",
"permissions": {
  "rule0": {
    "type": "io.cozy.files",
    "verbs": ["GET"],
    "values": ["foobar"]
  },
  "rule1": {
    "type": "cc.cozycloud.sentry",
    "verbs": ["POST"]
  }
},
"slug": "mini-test-perms",
"type": "webapp",
"version": "2.0.0"
}`
		}

		manifest3 := func() string {
			return `{
"description": "A mini app to test cozy-stack-v2",
"developer": {
  "name": "Cozy",
  "url": "cozy.io"
},
"license": "MIT",
"name": "mini-app",
"permissions": {
  "rule0": {
    "type": "io.cozy.files",
    "verbs": ["GET"],
    "values": ["foobar"]
  },
  "rule1": {
    "type": "cc.cozycloud.errors",
    "verbs": ["POST"]
  }
},
"slug": "mini-test-perms",
"type": "webapp",
"version": "3.0.0"
}`
		}

		manGen = manifest1
		manName = app.WebappManifestName
		finished := true

		instance, err := lifecycle.Create(&lifecycle.Options{
			Domain:             "test-keep-perms",
			OnboardingFinished: &finished,
		})
		assert.NoError(t, err)

		defer func() { _ = lifecycle.Destroy("test-keep-perms") }()

		inst, err := app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "mini-test-perms",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		man, err := inst.RunSync()
		assert.NoError(t, err)
		assert.Contains(t, man.Version(), "1.0.0")

		// Altering permissions by adding a value and a verb
		newPerms, err := permission.UnmarshalScopeString("io.cozy.files:GET,POST:foobar,foobar2 cc.cozycloud.sentry:POST")
		assert.NoError(t, err)

		customRule := permission.Rule{
			Title:  "myCustomRule",
			Verbs:  permission.Verbs(permission.PUT),
			Type:   "io.cozy.custom",
			Values: []string{"myCustomValue"},
		}
		newPerms = append(newPerms, customRule)

		_, err = permission.UpdateWebappSet(instance, "mini-test-perms", newPerms)
		assert.NoError(t, err)

		p1, err := permission.GetForWebapp(instance, "mini-test-perms")
		assert.NoError(t, err)
		assert.False(t, p1.Permissions.HasSameRules(man.Permissions()))

		// Update the app
		manGen = manifest2
		inst2, err := app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.WebappType,
			Slug:      "mini-test-perms",
			SourceURL: gitURL,
		})
		assert.NoError(t, err)

		man, err = inst2.RunSync()
		assert.NoError(t, err)

		p2, err := permission.GetForWebapp(instance, "mini-test-perms")
		assert.NoError(t, err)
		assert.Contains(t, man.Version(), "2.0.0")
		// Assert the rules were kept
		assert.False(t, p2.Permissions.HasSameRules(man.Permissions()))
		assert.True(t, p1.Permissions.HasSameRules(p2.Permissions))

		// Update again the app
		manGen = manifest3
		inst3, err := app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation:        app.Update,
			Type:             consts.WebappType,
			Slug:             "mini-test-perms",
			SourceURL:        gitURL,
			PermissionsAcked: true,
		})
		assert.NoError(t, err)

		man, err = inst3.RunSync()
		assert.NoError(t, err)

		p3, err := permission.GetForWebapp(instance, "mini-test-perms")
		assert.NoError(t, err)
		assert.Contains(t, man.Version(), "3.0.0")
		assert.False(t, p3.Permissions.HasSameRules(man.Permissions()))
		// Assert that rule1 type has been changed
		sentry := permission.Rule{
			Type:  "cc.cozycloud.sentry",
			Title: "rule1",
			Verbs: permission.Verbs(permission.POST),
		}
		assert.False(t, p3.Permissions.RuleInSubset(sentry))
		errors := permission.Rule{
			Type:  "cc.cozycloud.errors",
			Title: "rule1",
			Verbs: permission.Verbs(permission.POST),
		}
		assert.True(t, p3.Permissions.RuleInSubset(errors))
	})

	t.Run("WebappUpgradeNotExist", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName
		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.WebappType,
			Slug:      "cozy-app-not-exist",
		})
		assert.Nil(t, inst)
		assert.Equal(t, app.ErrNotFound, err)

		inst, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Delete,
			Type:      consts.WebappType,
			Slug:      "cozy-app-not-exist",
		})
		assert.Nil(t, inst)
		assert.Equal(t, app.ErrNotFound, err)
	})

	t.Run("WebappInstallWithUpgrade", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName

		defer func() {
			localServices = ""
		}()

		localServices = `{
		"service1": {

			"type": "node",
			"file": "/services/service1.js",
			"trigger": "@cron 0 0 0 * * *"
		}
	}`

		doUpgrade(t, 1)

		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "cozy-app-b",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		man, err := inst.RunSync()
		assert.NoError(t, err)

		ok, err := afero.Exists(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"), []byte("1.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")
		version1 := man.Version()

		manWebapp := man.(*app.WebappManifest)
		if assert.NotNil(t, manWebapp.Services()["service1"]) {
			service1 := manWebapp.Services()["service1"]
			assert.Equal(t, "/services/service1.js", service1.File)
			assert.Equal(t, "@cron 0 0 0 * * *", service1.TriggerOptions)
			assert.Equal(t, "node", service1.Type)
			assert.NotEmpty(t, service1.TriggerID)
		}

		doUpgrade(t, 2)
		localServices = ""

		inst, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.WebappType,
			Slug:      "cozy-app-b",
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
		version2 := man.Version()

		t.Log("versions: ", version1, version2)

		ok, err = afero.Exists(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"), []byte("2.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")
		manWebapp = man.(*app.WebappManifest)
		assert.Nil(t, manWebapp.Services()["service1"])
	})

	t.Run("WebappUpdateServices", func(t *testing.T) {
		manifest1 := func() string {
			return `{
"description": "A mini app to test cozy-stack-v2",
"developer": {
  "name": "Cozy",
  "url": "cozy.io"
},
"license": "MIT",
"name": "mini-app",
"permissions": {
  "rule0": {
    "type": "io.cozy.files",
    "verbs": ["GET"]
  }
},
"services": {
  "dacc": {
    "file": "services/dacc/drive.js",
    "trigger": "@every 720h",
    "type": "node"
  },
  "qualificationMigration": {
    "debounce": "24h",
    "file": "services/qualificationMigration/drive.js",
    "trigger": "@event io.cozy.files:CREATED,UPDATED",
    "type": "node"
  }
},
"slug": "mini-test-services",
"type": "webapp",
"version": "1.0.0"
}`
		}

		// @every -> @monthly
		manifest2 := func() string {
			return `{
"description": "A mini app to test cozy-stack-v2",
"developer": {
  "name": "Cozy",
  "url": "cozy.io"
},
"license": "MIT",
"name": "mini-app",
"permissions": {
  "rule0": {
    "type": "io.cozy.files",
    "verbs": ["GET"]
  }
},
"services": {
  "dacc": {
    "file": "services/dacc/drive.js",
    "trigger": "@monthly on the 3-5 between 2pm and 7pm",
    "type": "node"
  },
  "qualificationMigration": {
    "debounce": "24h",
    "file": "services/qualificationMigration/drive.js",
    "trigger": "@event io.cozy.files:CREATED,UPDATED",
    "type": "node"
  }
},
"slug": "mini-test-services",
"type": "webapp",
"version": "2.0.0"
}`
		}

		// monthly arguments
		manifest3 := func() string {
			return `{
"description": "A mini app to test cozy-stack-v2",
"developer": {
  "name": "Cozy",
  "url": "cozy.io"
},
"license": "MIT",
"name": "mini-app",
"permissions": {
  "rule0": {
    "type": "io.cozy.files",
    "verbs": ["GET"]
  }
},
"services": {
  "dacc": {
    "file": "services/dacc/drive.js",
    "trigger": "@monthly on the 2-4 between 1pm and 6pm",
    "type": "node"
  },
  "qualificationMigration": {
    "debounce": "24h",
    "file": "services/qualificationMigration/drive.js",
    "trigger": "@event io.cozy.files:CREATED,UPDATED",
    "type": "node"
  }
},
"slug": "mini-test-services",
"type": "webapp",
"version": "3.0.0"
}`
		}

		manGen = manifest1
		manName = app.WebappManifestName
		finished := true

		instance, err := lifecycle.Create(&lifecycle.Options{
			Domain:             "test-update-services",
			OnboardingFinished: &finished,
		})
		require.NoError(t, err)

		defer func() { _ = lifecycle.Destroy("test-update-services") }()

		inst, err := app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "mini-test-services",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		man, err := inst.RunSync()
		require.NoError(t, err)
		assert.Contains(t, man.Version(), "1.0.0")

		jobsSystem := job.System()
		triggers, err := jobsSystem.GetAllTriggers(instance)
		require.NoError(t, err)
		nbTriggers := len(triggers)

		trigger := findTrigger(triggers, "@event")
		require.NotNil(t, trigger)
		assert.Equal(t, "24h", trigger.Infos().Debounce)
		trigger = findTrigger(triggers, "@every")
		require.NotNil(t, trigger)
		assert.Equal(t, "720h", trigger.Infos().Arguments)

		// Update the app
		manGen = manifest2
		inst2, err := app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.WebappType,
			Slug:      "mini-test-services",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		man, err = inst2.RunSync()
		require.NoError(t, err)
		assert.Contains(t, man.Version(), "2.0.0")

		triggers, err = jobsSystem.GetAllTriggers(instance)
		require.NoError(t, err)
		assert.Equal(t, nbTriggers, len(triggers))

		trigger = findTrigger(triggers, "@event")
		require.NotNil(t, trigger)
		assert.Equal(t, "24h", trigger.Infos().Debounce)
		trigger = findTrigger(triggers, "@every")
		assert.Nil(t, trigger)
		trigger = findTrigger(triggers, "@monthly")
		assert.NotNil(t, trigger)
		assert.Equal(t, "on the 3-5 between 2pm and 7pm", trigger.Infos().Arguments)

		// Update again the app
		manGen = manifest3
		inst3, err := app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.WebappType,
			Slug:      "mini-test-services",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		man, err = inst3.RunSync()
		require.NoError(t, err)
		assert.Contains(t, man.Version(), "3.0.0")

		triggers, err = jobsSystem.GetAllTriggers(instance)
		require.NoError(t, err)
		assert.Equal(t, nbTriggers, len(triggers))
		trigger = findTrigger(triggers, "@event")
		require.NotNil(t, trigger)
		assert.Equal(t, "24h", trigger.Infos().Debounce)
		trigger = findTrigger(triggers, "@monthly")
		assert.NotNil(t, trigger)
		assert.Equal(t, "on the 2-4 between 1pm and 6pm", trigger.Infos().Arguments)
	})

	t.Run("WebappInstallAndUpgradeWithBranch", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName
		doUpgrade(t, 3)

		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "local-cozy-mini-branch",
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

		ok, err := afero.Exists(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"), []byte("3.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")
		ok, err = afero.Exists(baseFS, path.Join(storedDir(man), "branch.br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The good branch was checked out")

		doUpgrade(t, 4)

		inst, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.WebappType,
			Slug:      "local-cozy-mini-branch",
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

		ok, err = afero.Exists(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"), []byte("4.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")
		ok, err = afero.Exists(baseFS, path.Join(storedDir(man), "branch.br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The good branch was checked out")

		doUpgrade(t, 5)

		inst, err = app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.WebappType,
			Slug:      "local-cozy-mini-branch",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		man, err = inst.RunSync()
		require.NoError(t, err)

		assert.Equal(t, gitURL, man.Source())

		ok, err = afero.Exists(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest is present")
		ok, err = compressedFileContainsBytes(baseFS, path.Join(storedDir(man), app.WebappManifestName+".br"), []byte("5.0.0"))
		assert.NoError(t, err)
		assert.True(t, ok, "The manifest has the right version")
		ok, err = afero.Exists(baseFS, path.Join(storedDir(man), "branch.br"))
		assert.NoError(t, err)
		assert.False(t, ok, "The good branch was checked out")
	})

	t.Run("WebappInstallFromGithub", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName
		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "github-cozy-mini",
			SourceURL: "git://github.com/nono/cozy-mini.git",
		})
		require.NoError(t, err)

		go inst.Run()

		var state app.State
		for {
			man, done, err := inst.Poll()
			require.NoError(t, err)

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
	})

	t.Run("WebappInstallFromHTTP", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName
		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "http-cozy-drive",
			SourceURL: "https://github.com/cozy/cozy-drive/archive/71e5cde66f754f986afc7111962ed2dd361bd5e4.tar.gz",
		})
		require.NoError(t, err)

		go inst.Run()

		var state app.State
		for {
			man, done, err := inst.Poll()
			require.NoError(t, err)

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
	})

	t.Run("WebappUpdateWithService", func(t *testing.T) {
		manifest1 := func() string {
			return ` {
"description": "A mini app to test cozy-stack-v2",
"developer": {
	"name": "Cozy",
	"url": "cozy.io"
},
"license": "MIT",
"name": "mini-app",
"permissions": {
  "rule0": {
	"type": "io.cozy.files",
	"verbs": ["GET"],
	"values": ["foobar"]
  }
},
"services": {
	"onOperationOrBillCreate": {
		"type": "node",
		"file": "onOperationOrBillCreate.js",
		"trigger": "@event io.cozy.bank.operations:CREATED io.cozy.bills:CREATED",
		"debounce": "3m"
	  }
},
"slug": "mini-test-service",
"type": "webapp",
"version": "1.0.0"
}`
		}

		manifest2 := func() string {
			return ` {
"description": "A mini app to test cozy-stack-v2",
"developer": {
	"name": "Cozy",
	"url": "cozy.io"
},
"license": "MIT",
"name": "mini-app",
"permissions": {
	"rule0": {
		"type": "io.cozy.files",
		"verbs": ["GET", "POST"],
		"values": ["foobar"]
	}
},
"services": {
	"onOperationOrBillCreate": {
		"type": "node",
		"file": "onOperationOrBillCreate.js",
		"trigger": "@event io.cozy.bank.operations:CREATED io.cozy.bills:CREATED",
		"debounce": "3m"
	  }
},
"slug": "mini-test-service",
"type": "webapp",
"version": "2.0.0"
}`
		}
		conf := config.GetConfig()
		conf.Contexts = map[string]interface{}{
			"default": map[string]interface{}{},
		}

		manGen = manifest1
		manName = app.WebappManifestName
		finished := true

		instance, err := lifecycle.Create(&lifecycle.Options{
			Domain:             "test-update-with-service",
			OnboardingFinished: &finished,
		})
		assert.NoError(t, err)

		defer func() { _ = lifecycle.Destroy("test-update-with-service") }()

		inst, err := app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "mini-test-service",
			SourceURL: gitURL,
		})
		require.NoError(t, err)

		man, err := inst.RunSync()
		assert.NoError(t, err)
		assert.Contains(t, man.Version(), "1.0.0")

		t1, err := couchdb.CountAllDocs(instance, consts.Triggers)
		assert.NoError(t, err)

		// Update the app, but with new perms. The app should stay on the same
		// version
		manGen = manifest2
		inst2, err := app.NewInstaller(instance, fs, &app.InstallerOptions{
			Operation: app.Update,
			Type:      consts.WebappType,
			Slug:      "mini-test-service",
			SourceURL: gitURL,
		})
		assert.NoError(t, err)

		man, err = inst2.RunSync()
		assert.NoError(t, err)
		t2, err := couchdb.CountAllDocs(instance, consts.Triggers)
		assert.NoError(t, err)

		assert.Contains(t, man.Version(), "1.0.0")

		assert.Equal(t, t1, t2)
	})

	t.Run("WebappUninstall", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName
		inst1, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "github-cozy-delete",
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
			Type:      consts.WebappType,
			Slug:      "github-cozy-delete",
		})
		require.NoError(t, err)

		_, err = inst2.RunSync()
		require.NoError(t, err)

		inst3, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Delete,
			Type:      consts.WebappType,
			Slug:      "github-cozy-delete",
		})
		assert.Nil(t, inst3)
		assert.Equal(t, app.ErrNotFound, err)
	})

	t.Run("WebappUninstallErrored", func(t *testing.T) {
		manGen = manifestWebapp
		manName = app.WebappManifestName

		inst1, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "github-cozy-delete-errored",
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
			Operation: app.Update,
			Type:      consts.WebappType,
			Slug:      "github-cozy-delete-errored",
			SourceURL: "git://foobar.does.not.exist/",
		})
		require.NoError(t, err)

		go inst2.Run()
		for {
			var done bool
			_, done, err = inst2.Poll()
			if done || err != nil {
				break
			}
		}
		require.Error(t, err)

		inst3, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Delete,
			Type:      consts.WebappType,
			Slug:      "github-cozy-delete-errored",
		})
		require.NoError(t, err)

		_, err = inst3.RunSync()
		require.NoError(t, err)

		inst4, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Delete,
			Type:      consts.WebappType,
			Slug:      "github-cozy-delete-errored",
		})
		assert.Nil(t, inst4)
		assert.Equal(t, app.ErrNotFound, err)
	})

	t.Run("WebappInstallBadType", func(t *testing.T) {
		manGen = manifestKonnector
		manName = app.KonnectorManifestName

		inst, err := app.NewInstaller(db, fs, &app.InstallerOptions{
			Operation: app.Install,
			Type:      consts.WebappType,
			Slug:      "cozy-bad-type",
			SourceURL: gitURL,
		})
		assert.NoError(t, err)
		_, err = inst.RunSync()
		assert.Error(t, err)
		assert.ErrorIs(t, err, app.ErrInvalidManifestForWebapp)
	})
}

func findTrigger(triggers []job.Trigger, typ string) job.Trigger {
	for _, trigger := range triggers {
		if trigger.Type() == typ {
			return trigger
		}
	}
	return nil
}
