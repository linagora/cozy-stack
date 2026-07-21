package sharings_test

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	"github.com/cozy/cozy-stack/web/editor"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/notes"
	"github.com/cozy/cozy-stack/web/permissions"
	"github.com/cozy/cozy-stack/web/sharings"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// Test helpers to reduce duplication across scenarios
type sharedDrivesEnv struct {
	// Instances
	acme, betty, dave *instance.Instance
	// Tokens
	acmeToken, bettyToken, daveToken string
	// Servers (closed via t.Cleanup)
	tsA, tsB, tsD *httptest.Server

	// Common resources
	firstSharingID string
	firstRootDirID string
	rootDirName    string // Name of the root directory for path construction
	productDirID   string
	meetingsDirID  string

	// Additional resources for drives_test.go
	checklistID      string // File inside meetingsDirID for file operation tests
	outsideOfShareID string // Directory outside the share for boundary tests
}

// createClients creates httpexpect clients for the current test with proper error reporting
func (env *sharedDrivesEnv) createClients(t *testing.T) (*httpexpect.Expect, *httpexpect.Expect, *httpexpect.Expect) {
	t.Helper()
	eA := newSharingExpect(t, env.tsA.URL)
	eB := newSharingExpect(t, env.tsB.URL)
	eD := newSharingExpect(t, env.tsD.URL)
	return eA, eB, eD
}

func setupSharedDrivesEnv(t *testing.T) *sharedDrivesEnv {
	return setupSharedDrivesEnvWithOwnerOptions(t, nil)
}

func setupSharedDrivesEnvWithOwnerOptions(t *testing.T, ownerOptions *lifecycle.Options) *sharedDrivesEnv {
	t.Helper()

	config.UseTestFile(t)
	build.BuildMode = build.ModeDev
	config.GetConfig().Assets = "../../assets"
	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()
	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()))

	testName := strings.ReplaceAll(t.Name(), "/", "_")
	acme, acmeToken, tsA := newDriveOwnerTestServerWithOptions(
		t,
		testName+"_acme",
		ownerOptions,
		render,
		map[string]func(*echo.Group){
			"/editor":      editor.Routes,
			"/notes":       notes.Routes,
			"/permissions": permissions.Routes,
		},
	)
	betty, bettyToken, tsB := newDriveRecipientTestServer(
		t,
		testName+"_betty",
		"betty@example.net",
		"Betty",
		render,
		nil,
	)
	dave, daveToken, tsD := newDriveRecipientTestServer(
		t,
		testName+"_dave",
		"dave@example.net",
		"Dave",
		render,
		nil,
	)

	// Create initial shared drive and accept as Betty; create common dirs
	rootDirName := "One More Shared Drive " + crypto.GenerateRandomString(1000)
	sharingID, firstRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, acme, acmeToken, tsA.URL,
		rootDirName, "One more Shared drive description", nil)
	acceptSharedDriveForBetty(t, acme, betty, tsA.URL, tsB.URL, sharingID)

	// Create temporary clients for initial setup
	eA := newSharingExpect(t, tsA.URL)

	productDirID := createDirectory(t, eA, firstRootDirID, "Product", acmeToken)
	meetingsDirID := createDirectory(t, eA, firstRootDirID, "Meetings", acmeToken)

	// Create a file inside meetingsDirID for file operation tests
	checklistID := createFile(t, eA, meetingsDirID, "Checklist.txt", acmeToken)

	// Create a directory outside the share for boundary tests
	outsideOfShareID := createRootDirectory(t, eA, "Unshared directory at the root of ACME", acmeToken)

	return &sharedDrivesEnv{
		acme: acme, betty: betty, dave: dave,
		acmeToken: acmeToken, bettyToken: bettyToken, daveToken: daveToken,
		tsA: tsA, tsB: tsB, tsD: tsD,
		firstSharingID: sharingID, firstRootDirID: firstRootDirID, rootDirName: rootDirName,
		productDirID: productDirID, meetingsDirID: meetingsDirID,
		checklistID: checklistID, outsideOfShareID: outsideOfShareID,
	}
}

func forceCrossStack(t *testing.T, baseURL string) func() {
	t.Helper()
	prevLocal := sharings.LocalInstanceFromURL
	prevClient := sharings.NewRemoteClient
	u, _ := url.Parse(baseURL)
	sharings.LocalInstanceFromURL = func(rawURL string) *instance.Instance {
		return nil
	}
	sharings.NewRemoteClient = mockAcmeClient(u)
	return func() {
		sharings.LocalInstanceFromURL = prevLocal
		sharings.NewRemoteClient = prevClient
	}
}

// Convenience wrapper to POST to /sharings/drives/move and return the JSON object.
func postMove(t *testing.T, e *httpexpect.Expect, token string, body string) *httpexpect.Object {
	t.Helper()
	return e.POST("/sharings/drives/move").
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(body)).
		Expect().Status(201).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()
}

// postMoveExpectStatus posts a move request and asserts the provided HTTP status.
// Returns the raw response object for further assertions when needed.
func postMoveExpectStatus(t *testing.T, e *httpexpect.Expect, token string, body string, status int) *httpexpect.Response {
	t.Helper()
	return e.POST("/sharings/drives/move").
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/json").
		WithBytes([]byte(body)).
		Expect().Status(status)
}

func TestSharedDrivesMove(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	t.Run("SuccessfulMove_ToSharedDrive_SameStack", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		// Perform the move operation
		fileToMoveSameStack := createFile(t, eB, "", "file-to-upload.txt", env.bettyToken)

		// Pre-fetch the source FileDoc before the move so we can verify physical deletion later
		srcFileDoc, err := env.betty.VFS().FileByID(fileToMoveSameStack)
		require.NoError(t, err)

		responseObj := postMove(t, eB, env.bettyToken, `{
				  "source": {
				    "file_id": "`+fileToMoveSameStack+`"
				  },
				  "dest": {
				    "instance": "https://`+env.acme.Domain+`",
				    "sharing_id": "`+env.firstSharingID+`",
				    "dir_id": "`+env.productDirID+`"
				  }
				}`)

		// Verify the response and get moved file ID
		movedFileID := assertMoveResponseWithSharing(t, responseObj, "file-to-upload.txt", env.productDirID, env.firstSharingID)

		// Verify the file was moved and content preserved
		verifyFileMove(t, env.acme, movedFileID, "file-to-upload.txt", env.productDirID, "foo")

		// Verify the original file was deleted (both metadata and physical content)
		verifyFileDeleted(t, env.betty, srcFileDoc)
	})

	// Force the cross-stack path even if instances are on the same server
	t.Run("SuccessfulMove_ToSharedDrive_DifferentStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		fileToMoveDifferentStack := createFile(t, eB, "", "file-to-upload-diff.txt", env.bettyToken)
		destDirInSharedDrive := createDirectory(t, eA, env.productDirID, "Dest DIR To Move", env.acmeToken)

		// Pre-fetch the source FileDoc before the move so we can verify physical deletion later
		srcFileDoc, err := env.betty.VFS().FileByID(fileToMoveDifferentStack)
		require.NoError(t, err)

		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		responseObj := postMove(t, eB, env.bettyToken, `{
				  "source": {
				    "file_id": "`+fileToMoveDifferentStack+`"
				  },
				  "dest": {
				    "instance": "https://`+env.acme.Domain+`",
				    "sharing_id": "`+env.firstSharingID+`",
				    "dir_id": "`+destDirInSharedDrive+`"
				  }
				}`)

		// Verify the response and get moved file ID
		movedFileID := assertMoveResponseWithSharing(t, responseObj, "file-to-upload-diff.txt", destDirInSharedDrive, env.firstSharingID)

		// Verify the file was moved and content preserved
		verifyFileMove(t, env.acme, movedFileID, "file-to-upload-diff.txt", destDirInSharedDrive, "foo")

		// Verify the original file was deleted from betty (both metadata and physical content)
		verifyFileDeleted(t, env.betty, srcFileDoc)
	})

	t.Run("SuccessfulMove_FromSharedDrive_SameStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		// Create file to move
		fileToMove := createFile(t, eA, env.meetingsDirID, "file-to-move-upstream.txt", env.acmeToken)
		// Create destination directory on the target instance
		destDirID := createRootDirectory(t, eB, "Destination Dir", env.bettyToken)

		// Pre-fetch the source FileDoc before the move so we can verify physical deletion later
		srcFileDoc, err := env.acme.VFS().FileByID(fileToMove)
		require.NoError(t, err)

		responseObj := postMove(t, eB, env.bettyToken, `{
				  "source": {
					"instance": "https://`+env.acme.Domain+`",
					 "sharing_id": "`+env.firstSharingID+`",
				    "file_id": "`+fileToMove+`"
				  },
				  "dest": {
				    "dir_id": "`+destDirID+`"
				  }
				}`)

		// Verify the response and get moved file ID
		movedFileID := assertMoveResponse(t, responseObj, "file-to-move-upstream.txt", destDirID)

		// Verify the file was moved to the destination
		verifyFileMove(t, env.betty, movedFileID, "file-to-move-upstream.txt", destDirID, "foo")

		// Verify the original file was deleted (both metadata and physical content)
		verifyFileDeleted(t, env.acme, srcFileDoc)
	})

	// Force the cross-stack path even if instances are on the same server
	t.Run("SuccessfulMove_FromSharedDrive_DifferentStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		fileName := "file-to-move-diff.txt"
		fileToDiffStack := createFile(t, eA, env.meetingsDirID, fileName, env.acmeToken)
		// Create destination directory on the target (owner) instance
		destDirID := createRootDirectory(t, eB, "Destination Dir Diff", env.bettyToken)

		// Pre-fetch the source FileDoc before the move so we can verify physical deletion later
		srcFileDoc, err := env.acme.VFS().FileByID(fileToDiffStack)
		require.NoError(t, err)

		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		responseObj := postMove(t, eB, env.bettyToken, `{
				  "source": {
					"instance": "https://`+env.acme.Domain+`",
					 "sharing_id": "`+env.firstSharingID+`",
				    "file_id": "`+fileToDiffStack+`"
				  },
				  "dest": {
				    "dir_id": "`+destDirID+`"
				  }
				}`)

		// Verify the response and get moved file ID
		movedFileID := assertMoveResponse(t, responseObj, fileName, destDirID)

		// Verify the file was moved to the destination
		verifyFileMove(t, env.betty, movedFileID, fileName, destDirID, "foo")

		// Verify the original file was deleted from source (both metadata and physical content)
		verifyFileDeleted(t, env.acme, srcFileDoc)
	})

	t.Run("SuccessfulMove_BetweenSharedDrives_DifferentStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"One More Shared Drive", "One more Shared drive description", nil)
		// Accept it as Betty
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Create the file on the owner (ACME) instance inside the second shared drive
		fileName := "file-to-move-between-shared-drives.txt"
		toMove := createFile(t, eA, secondRootDirID, fileName, env.acmeToken)

		// Pre-fetch the source FileDoc before the move so we can verify physical deletion later
		srcFileDoc, err := env.acme.VFS().FileByID(toMove)
		require.NoError(t, err)

		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		responseObj := postMove(t, eB, env.bettyToken, `{
				  "source": {
					 "instance": "https://`+env.acme.Domain+`",
					 "sharing_id": "`+secondSharingID+`",
				     "file_id": "`+toMove+`"
				  },
				  "dest": {
                     "instance": "https://`+env.acme.Domain+`",
				     "dir_id": "`+env.meetingsDirID+`",
					 "sharing_id": "`+env.firstSharingID+`"
				  }
				}`)

		// Verify the response contains the new file
		movedFileID := assertMoveResponseWithSharing(t, responseObj, fileName, env.meetingsDirID, env.firstSharingID)

		// Verify the file was moved to the destination
		verifyFileMove(t, env.acme, movedFileID, fileName, env.meetingsDirID, "foo")
		// Verify the original file was deleted (both metadata and physical content)
		verifyFileDeleted(t, env.acme, srcFileDoc)
	})

	t.Run("SuccessfulMoveBetween_SharedDrives_SameStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		firstSharingID, firstRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"One More Shared Drive "+crypto.GenerateRandomString(1000), "One more Shared drive description", nil)
		// Accept it as Betty
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, firstSharingID)

		meetingsID := eA.POST("/files/"+firstRootDirID).
			WithQuery("Name", "Meetings").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+env.acmeToken).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"One More Shared Drive "+crypto.GenerateRandomString(1000), "One more Shared drive description", nil)
		// Accept it as Betty
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Create the file on the owner (ACME) instance inside the second shared drive
		fileName := "file-to-move-between-shared-drives.txt" + crypto.GenerateRandomString(1000)
		toMove := createFile(t, eA, secondRootDirID, fileName, env.acmeToken)

		responseObj := postMove(t, eB, env.bettyToken, `{
				  "source": {
					 "instance": "https://`+env.acme.Domain+`",
					 "sharing_id": "`+secondSharingID+`",
				     "file_id": "`+toMove+`"
				  },
				  "dest": {
                     "instance": "https://`+env.acme.Domain+`",
				     "dir_id": "`+meetingsID+`",
					 "sharing_id": "`+firstSharingID+`"
				  }
				}`)

		// Verify the response contains the new file
		movedFileID := assertMoveResponseWithSharing(t, responseObj, fileName, meetingsID, firstSharingID)

		// Verify the file was moved to the destination.
		// Same-instance move preserves the file ID (metadata-only update via ModifyFileMetadata).
		require.Equal(t, toMove, movedFileID, "same-instance move must preserve the file ID")
		verifyFileMove(t, env.acme, movedFileID, fileName, meetingsID, "foo")
	})

	// File move with conflict resolution: moving a file between shared drives (same stack)
	// when a file with the same name already exists at destination should auto-rename.
	t.Run("AutoRename_MoveFile_BetweenSharedDrives_SameStack_NameExists", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Prepare: create another shared drive and accept it as Betty (source drive)
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"AutoRename Target Drive "+strings.ReplaceAll(t.Name(), "/", "_"), "Drive used as source for auto-rename move", nil)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Create destination file with the conflicting name in the first shared drive (target)
		conflictName := "conflict-name-" + strings.ReplaceAll(t.Name(), "/", "_") + ".txt"
		_ = createFile(t, eA, env.meetingsDirID, conflictName, env.acmeToken)

		// Create source file with the same name in the second shared drive (source)
		sourceFileID := createFile(t, eA, secondRootDirID, conflictName, env.acmeToken)

		// Attempt to move → should succeed with auto-renaming
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+secondSharingID+`",
			    "file_id": "`+sourceFileID+`"
			  },
			  "dest": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+env.firstSharingID+`",
			    "dir_id": "`+env.meetingsDirID+`"
			  }
			}`)

		// Verify the file was moved with a renamed name (conflict resolution)
		movedFileName := assertAutoRename(t, responseObj, conflictName)
		movedFileID := responseObj.Path("$.data.id").String().Raw()

		// Verify both files exist in destination (original + renamed)
		verifyBothFilesExist(t, env.acme, env.meetingsDirID, conflictName, movedFileName)

		// Same-instance move preserves the file ID (metadata-only update via ModifyFileMetadata).
		// The source file was moved (not deleted): verify it now lives at the destination with the renamed name.
		require.Equal(t, sourceFileID, movedFileID, "same-instance move must preserve the file ID")
		verifyFileExists(t, env.acme, sourceFileID, movedFileName, env.meetingsDirID, "foo")
	})

	// File move with conflict resolution: moving a file between shared drives (same stack)
	// when a file with the same name already exists at destination should auto-rename.
	t.Run("AutoRename_MoveFile_BetweenSharedDrives_DifferentStack_NameExists", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		// Prepare: create another shared drive and accept it as Betty (source drive)
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"AutoRename Target Drive "+strings.ReplaceAll(t.Name(), "/", "_"), "Drive used as source for auto-rename move", nil)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Create destination file with the conflicting name in the first shared drive (target)
		conflictName := "conflict-name-" + strings.ReplaceAll(t.Name(), "/", "_") + ".txt"
		_ = createFile(t, eA, env.meetingsDirID, conflictName, env.acmeToken)

		// Create source file with the same name in the second shared drive (source)
		sourceFileID := createFile(t, eA, secondRootDirID, conflictName, env.acmeToken)

		// Pre-fetch the source FileDoc before the move so we can verify physical deletion later
		srcFileDoc, err := env.acme.VFS().FileByID(sourceFileID)
		require.NoError(t, err)

		// Attempt to move → should succeed with auto-renaming
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+secondSharingID+`",
			    "file_id": "`+sourceFileID+`"
			  },
			  "dest": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+env.firstSharingID+`",
			    "dir_id": "`+env.meetingsDirID+`"
			  }
			}`)

		// Verify the file was moved with a renamed name (conflict resolution)
		movedFileName := assertAutoRename(t, responseObj, conflictName)

		// Verify both files exist in destination (original + renamed)
		verifyBothFilesExist(t, env.acme, env.meetingsDirID, conflictName, movedFileName)

		// Verify source file was deleted (both metadata and physical content)
		verifyFileDeleted(t, env.acme, srcFileDoc)
	})

	// Folder move with conflict resolution: moving a directory between shared drives (same stack)
	// when a directory with the same name already exists at destination should auto-rename.
	t.Run("AutoRename_MoveDir_BetweenSharedDrives_SameStack_NameExists", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Prepare: create another shared drive and accept it as Betty (source drive)
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"AutoRename Target Drive "+strings.ReplaceAll(t.Name(), "/", "_"), "Drive used as source for auto-rename move", nil)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Create destination file with the conflicting name in the first shared drive (target)
		conflictName := "conflict-name-" + strings.ReplaceAll(t.Name(), "/", "_")
		_ = createDirectory(t, eA, env.meetingsDirID, conflictName, env.acmeToken)

		// Create source file with the same name in the second shared drive (source)
		sourceDirID := createDirectory(t, eA, secondRootDirID, conflictName, env.acmeToken)

		// Attempt to move → should succeed with auto-renaming
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+secondSharingID+`",
			    "dir_id": "`+sourceDirID+`"
			  },
			  "dest": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+env.firstSharingID+`",
			    "dir_id": "`+env.meetingsDirID+`"
			  }
			}`)

		// Verify the file was moved with a renamed name (conflict resolution)
		movedFileName := assertAutoRename(t, responseObj, conflictName)

		// Verify both files exist in destination (original + renamed)
		verifyBothFilesExist(t, env.acme, env.meetingsDirID, conflictName, movedFileName)
	})

	// Folder move with conflict resolution: moving a directory between shared drives (same stack)
	// when a directory with the same name already exists at destination should auto-rename.
	t.Run("AutoRename_MoveDir_BetweenSharedDrives_DifferentStack_NameExists", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		// Prepare: create another shared drive and accept it as Betty (source drive)
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"AutoRename Target Drive "+strings.ReplaceAll(t.Name(), "/", "_"), "Drive used as source for auto-rename move", nil)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Create destination file with the conflicting name in the first shared drive (target)
		conflictName := "conflict-name-" + strings.ReplaceAll(t.Name(), "/", "_")
		_ = createDirectory(t, eA, env.meetingsDirID, conflictName, env.acmeToken)

		// Create source file with the same name in the second shared drive (source)
		sourceDirID := createDirectory(t, eA, secondRootDirID, conflictName, env.acmeToken)

		// Attempt to move → should succeed with auto-renaming
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+secondSharingID+`",
			    "dir_id": "`+sourceDirID+`"
			  },
			  "dest": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+env.firstSharingID+`",
			    "dir_id": "`+env.meetingsDirID+`"
			  }
			}`)

		// Verify the file was moved with a renamed name (conflict resolution)
		movedFileName := assertAutoRename(t, responseObj, conflictName)

		// Verify both files exist in destination (original + renamed)
		verifyBothFilesExist(t, env.acme, env.meetingsDirID, conflictName, movedFileName)

		// Verify source dir was deleted
		verifyNodeDeleted(t, env.acme, sourceDirID)
	})

	// Move directory from Betty's local drive to a shared drive
	t.Run("MoveDirectoryWithFilesAndChild_LocalToSharedDrive_SameStack", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		localTestDirIDName := "LocalTestDir" + strings.ReplaceAll(t.Name(), "/", "_")
		// Create a directory with files and subdirectories in Betty's local drive
		localDirID := createDirectory(t, eB, "", localTestDirIDName, env.bettyToken)
		_ = createFile(t, eB, localDirID, "local-file1.txt", env.bettyToken)
		_ = createFile(t, eB, localDirID, "local-file2.md", env.bettyToken)
		subDirID := createDirectory(t, eB, localDirID, "LocalSubDir", env.bettyToken)
		_ = createFile(t, eB, subDirID, "local-file3.bin", env.bettyToken)

		// Move the directory to the shared drive
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {
			    "dir_id": "`+localDirID+`"
			  },
			  "dest": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+env.firstSharingID+`",
			    "dir_id": "`+env.meetingsDirID+`"
			  }
			}`)

		// Verify the directory was moved successfully
		assertDirectoryResponse(t, responseObj, localTestDirIDName, env.meetingsDirID)

		// Get the actual path of the meetings directory
		meetingsDir, err := env.acme.VFS().DirByID(env.meetingsDirID)
		require.NoError(t, err)

		// Verify the moved directory exists in the shared drive
		movedDir, err := env.acme.VFS().DirByPath(meetingsDir.Fullpath + "/" + localTestDirIDName)
		require.NoError(t, err)
		require.Equal(t, localTestDirIDName, movedDir.DocName)

		// Verify all files were moved to the shared drive
		_, err = env.acme.VFS().FileByPath(meetingsDir.Fullpath + "/" + localTestDirIDName + "/local-file1.txt")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(meetingsDir.Fullpath + "/" + localTestDirIDName + "/local-file2.md")
		require.NoError(t, err)

		// Verify subdirectory and its file were moved
		_, err = env.acme.VFS().DirByPath(meetingsDir.Fullpath + "/" + localTestDirIDName + "/LocalSubDir")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(meetingsDir.Fullpath + "/" + localTestDirIDName + "/LocalSubDir/local-file3.bin")
		require.NoError(t, err)

		// Verify the original directory was deleted from Betty's local drive
		verifyNodeDeleted(t, env.betty, localDirID)
	})

	// Auto-rename on directory root conflict: Local -> Shared (same stack)
	t.Run("AutoRename_MoveDir_LocalToShared_SameStack_NameExists", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		conflictName := "ConflictingDir" + strings.ReplaceAll(t.Name(), "/", "_")

		// Destination already has a directory with the same name in the shared drive
		// Create via ACME API (owner of the shared drive)
		eA := httpexpect.WithConfig(httpexpect.Config{BaseURL: env.tsA.URL, Reporter: httpexpect.NewRequireReporter(t), Printers: []httpexpect.Printer{httpexpect.NewCompactPrinter(t)}})
		_ = createDirectory(t, eA, env.meetingsDirID, conflictName, env.acmeToken)

		// Create a local directory with the same name on Betty's drive
		localDirID := createDirectory(t, eB, "", conflictName, env.bettyToken)
		_ = createFile(t, eB, localDirID, "local.txt", env.bettyToken)

		// Move local directory into the shared drive (should auto-rename)
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {"dir_id": "`+localDirID+`"},
			  "dest": {"instance": "https://`+env.acme.Domain+`", "sharing_id": "`+env.firstSharingID+`", "dir_id": "`+env.meetingsDirID+`"}
			}`)

		movedName := assertAutoRename(t, responseObj, conflictName)

		// Verify both directories exist on destination
		verifyBothFilesExist(t, env.acme, env.meetingsDirID, conflictName, movedName)
	})

	// Auto-rename on directory root conflict: Shared -> Local (same stack)
	t.Run("AutoRename_MoveDir_SharedToLocal_SameStack_NameExists", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		conflictName := "ConflictingDir" + strings.ReplaceAll(t.Name(), "/", "_")

		// Destination local root and a conflicting child directory
		destRoot := createRootDirectory(t, eB, "LocalDestRoot"+strings.ReplaceAll(t.Name(), "/", "_"), env.bettyToken)
		_ = createDirectory(t, eB, destRoot, conflictName, env.bettyToken)

		// Source directory in shared drive with the same name
		srcDir := createDirectory(t, eA, env.meetingsDirID, conflictName, env.acmeToken)
		_ = createFile(t, eA, srcDir, "shared.txt", env.acmeToken)

		// Move shared directory into local destination (should auto-rename)
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {"instance": "https://`+env.acme.Domain+`", "sharing_id": "`+env.firstSharingID+`", "dir_id": "`+srcDir+`"},
			  "dest": {"dir_id": "`+destRoot+`"}
			}`)

		movedName := assertAutoRename(t, responseObj, conflictName)

		// Verify both directories exist locally
		verifyBothFilesExist(t, env.betty, destRoot, conflictName, movedName)
	})

	// Move directory from Betty's local drive to a shared drive
	t.Run("MoveDirectoryWithFilesAndChild_LocalToSharedDrive_DifferentStack", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		// Create a directory with files and subdirectories in Betty's local drive
		localDirID := createDirectory(t, eB, "", "LocalTestDir", env.bettyToken)
		_ = createFile(t, eB, localDirID, "local-file1.txt", env.bettyToken)
		_ = createFile(t, eB, localDirID, "local-file2.md", env.bettyToken)
		subDirID := createDirectory(t, eB, localDirID, "LocalSubDir", env.bettyToken)
		_ = createFile(t, eB, subDirID, "local-file3.bin", env.bettyToken)

		// Move the directory to the shared drive
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {
			    "dir_id": "`+localDirID+`"
			  },
			  "dest": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+env.firstSharingID+`",
			    "dir_id": "`+env.meetingsDirID+`"
			  }
			}`)

		// Verify the directory was moved successfully
		assertDirectoryResponse(t, responseObj, "LocalTestDir", env.meetingsDirID)

		// Get the actual path of the meetings directory
		meetingsDir, err := env.acme.VFS().DirByID(env.meetingsDirID)
		require.NoError(t, err)

		// Verify the moved directory exists in the shared drive
		movedDir, err := env.acme.VFS().DirByPath(meetingsDir.Fullpath + "/LocalTestDir")
		require.NoError(t, err)
		require.Equal(t, "LocalTestDir", movedDir.DocName)

		// Verify all files were moved to the shared drive
		_, err = env.acme.VFS().FileByPath(meetingsDir.Fullpath + "/LocalTestDir/local-file1.txt")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(meetingsDir.Fullpath + "/LocalTestDir/local-file2.md")
		require.NoError(t, err)

		// Verify subdirectory and its file were moved
		_, err = env.acme.VFS().DirByPath(meetingsDir.Fullpath + "/LocalTestDir/LocalSubDir")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(meetingsDir.Fullpath + "/LocalTestDir/LocalSubDir/local-file3.bin")
		require.NoError(t, err)

		// Verify the original directory was deleted from Betty's local drive
		verifyNodeDeleted(t, env.betty, localDirID)
	})

	// Move directory from a shared drive to Betty's local drive
	t.Run("MoveDirectoryWithFilesAndChild_SharedDriveToLocal_SameStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Create a directory with files and subdirectories in the shared drive
		sharedDirID := createDirectory(t, eA, env.meetingsDirID, "SharedTestDir", env.acmeToken)
		_ = createFile(t, eA, sharedDirID, "shared-file1.txt", env.acmeToken)
		_ = createFile(t, eA, sharedDirID, "shared-file2.md", env.acmeToken)
		subDirID := createDirectory(t, eA, sharedDirID, "SharedSubDir", env.acmeToken)
		_ = createFile(t, eA, subDirID, "shared-file3.bin", env.acmeToken)

		// Create a destination directory in Betty's local drive
		destDirID := createRootDirectory(t, eB, "Betty Dest Dir", env.bettyToken)

		// Move the directory to Betty's local drive
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+env.firstSharingID+`",
			    "dir_id": "`+sharedDirID+`"
			  },
			  "dest": {
			    "dir_id": "`+destDirID+`"
			  }
			}`)

		// Verify the directory was moved successfully
		assertDirectoryResponse(t, responseObj, "SharedTestDir", destDirID)

		// Get the actual path of the destination directory
		destDir, err := env.betty.VFS().DirByID(destDirID)
		require.NoError(t, err)

		// Verify the moved directory exists in Betty's local drive
		movedDir, err := env.betty.VFS().DirByPath(destDir.Fullpath + "/SharedTestDir")
		require.NoError(t, err)
		require.Equal(t, "SharedTestDir", movedDir.DocName)

		// Verify all files were moved to Betty's local drive
		_, err = env.betty.VFS().FileByPath(destDir.Fullpath + "/SharedTestDir/shared-file1.txt")
		require.NoError(t, err)
		_, err = env.betty.VFS().FileByPath(destDir.Fullpath + "/SharedTestDir/shared-file2.md")
		require.NoError(t, err)

		// Verify subdirectory and its file were moved
		_, err = env.betty.VFS().DirByPath(destDir.Fullpath + "/SharedTestDir/SharedSubDir")
		require.NoError(t, err)
		_, err = env.betty.VFS().FileByPath(destDir.Fullpath + "/SharedTestDir/SharedSubDir/shared-file3.bin")
		require.NoError(t, err)

		// Verify the original directory was deleted from the shared drive
		verifyNodeDeleted(t, env.acme, sharedDirID)
	})

	// Move directory from a shared drive to Betty's local drive
	t.Run("MoveDirectoryWithFilesAndChild_SharedDriveToLocal_DifferentStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		// Create a directory with files and subdirectories in the shared drive
		sharedTestDirName := "SharedTestDir" + strings.ReplaceAll(t.Name(), "/", "_")
		sharedDirID := createDirectory(t, eA, env.meetingsDirID, sharedTestDirName, env.acmeToken)
		_ = createFile(t, eA, sharedDirID, "shared-file1.txt", env.acmeToken)
		_ = createFile(t, eA, sharedDirID, "shared-file2.md", env.acmeToken)
		subDirID := createDirectory(t, eA, sharedDirID, "SharedSubDir", env.acmeToken)
		_ = createFile(t, eA, subDirID, "shared-file3.bin", env.acmeToken)

		// Create a destination directory in Betty's local drive
		destDirID := createRootDirectory(t, eB, "Betty Dest Dir"+strings.ReplaceAll(t.Name(), "/", "_"), env.bettyToken)

		// Move the directory to Betty's local drive
		responseObj := postMove(t, eB, env.bettyToken, `{
			  "source": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+env.firstSharingID+`",
			    "dir_id": "`+sharedDirID+`"
			  },
			  "dest": {
			    "dir_id": "`+destDirID+`"
			  }
			}`)

		// Verify the directory was moved successfully
		assertDirectoryResponse(t, responseObj, sharedTestDirName, destDirID)

		// Get the actual path of the destination directory
		destDir, err := env.betty.VFS().DirByID(destDirID)
		require.NoError(t, err)

		// Verify the moved directory exists in Betty's local drive
		movedDir, err := env.betty.VFS().DirByPath(destDir.Fullpath + "/" + sharedTestDirName)
		require.NoError(t, err)
		require.Equal(t, sharedTestDirName, movedDir.DocName)

		// Verify all files were moved to Betty's local drive
		_, err = env.betty.VFS().FileByPath(destDir.Fullpath + "/" + sharedTestDirName + "/shared-file1.txt")
		require.NoError(t, err)
		_, err = env.betty.VFS().FileByPath(destDir.Fullpath + "/" + sharedTestDirName + "/shared-file2.md")
		require.NoError(t, err)

		// Verify subdirectory and its file were moved
		_, err = env.betty.VFS().DirByPath(destDir.Fullpath + "/" + sharedTestDirName + "/SharedSubDir")
		require.NoError(t, err)
		_, err = env.betty.VFS().FileByPath(destDir.Fullpath + "/" + sharedTestDirName + "/SharedSubDir/shared-file3.bin")
		require.NoError(t, err)

		// Verify the original directory was deleted from the shared drive
		verifyNodeDeleted(t, env.acme, sharedDirID)
	})

	t.Run("MoveDirectoryWithFilesAndChild_BetweenSharedDrives_SameStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		// Prepare: create a second shared drive and accept it as Betty
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"NestedDir Move Target Drive", "Drive used as destination for nested dir move", nil)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Prepare: create a directory with files and a child directory with files in the first drive
		srcDirID := createDirectory(t, eA, env.productDirID, "FolderToMove", env.acmeToken)
		_ = createFile(t, eA, srcDirID, "A1.txt", env.acmeToken)
		_ = createFile(t, eA, srcDirID, "B1.md", env.acmeToken)
		_ = createFile(t, eA, srcDirID, "C1.bin", env.acmeToken)
		childDirID := createDirectory(t, eA, srcDirID, "SubFolder", env.acmeToken)
		_ = createFile(t, eA, childDirID, "A2.txt", env.acmeToken)
		// add deeper hierarchy
		deepDirID := createDirectory(t, eA, childDirID, "Deep", env.acmeToken)
		_ = createFile(t, eA, deepDirID, "D1.txt", env.acmeToken)

		// Destination directory under the second shared drive
		destDirID := createDirectory(t, eA, secondRootDirID, "DestForFolder", env.acmeToken)

		// Move the directory subtree
		postMove(t, eB, env.bettyToken, `{
				  "source": {
				    "instance": "https://`+env.acme.Domain+`",
				    "sharing_id": "`+env.firstSharingID+`",
				    "dir_id": "`+srcDirID+`"
				  },
				  "dest": {
				    "instance": "https://`+env.acme.Domain+`",
				    "sharing_id": "`+secondSharingID+`",
				    "dir_id": "`+destDirID+`"	
				  }
				}`)

		// Verify using VFS on owner instance (IDs are not preserved; check by names and paths)
		// 1) Destination now contains a directory named like the source root
		destRoot, err := env.acme.VFS().DirByID(destDirID)
		require.NoError(t, err)
		movedRoot, err := env.acme.VFS().DirByPath(destRoot.Fullpath + "/" + "FolderToMove")
		require.NoError(t, err)
		require.Equal(t, "FolderToMove", movedRoot.DocName)

		// 2) Moved root contains multiple files and subdir SubFolder
		_, err = env.acme.VFS().FileByPath(movedRoot.Fullpath + "/A1.txt")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(movedRoot.Fullpath + "/B1.md")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(movedRoot.Fullpath + "/C1.bin")
		require.NoError(t, err)
		childDir, err := env.acme.VFS().DirByPath(movedRoot.Fullpath + "/SubFolder")
		require.NoError(t, err)

		// 3) Child dir contains A2.txt and nested Deep/D1.txt
		_, err = env.acme.VFS().FileByPath(childDir.Fullpath + "/A2.txt")
		require.NoError(t, err)
		deepDir, err := env.acme.VFS().DirByPath(childDir.Fullpath + "/Deep")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(deepDir.Fullpath + "/D1.txt")
		require.NoError(t, err)

		// 4) Original source path no longer exists
		productRoot, err := env.acme.VFS().DirByID(env.productDirID)
		require.NoError(t, err)
		_, err = env.acme.VFS().DirByPath(productRoot.Fullpath + "/FolderToMove")
		require.Error(t, err)
	})

	t.Run("MoveDirectoryWithFilesAndChild_BetweenSharedDrives_DifferentStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)
		// Prepare: create a second shared drive and accept it as Betty
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"NestedDir Move Target Drive Different Stack", "Drive used as destination for nested dir move", nil)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Prepare: create a directory with files and a child directory with files in the first drive
		dirToMoveName := "FolderToMove" + strings.ReplaceAll(t.Name(), "/", "_")
		srcDirID := createDirectory(t, eA, env.productDirID, dirToMoveName, env.acmeToken)
		_ = createFile(t, eA, srcDirID, "A1.txt", env.acmeToken)
		_ = createFile(t, eA, srcDirID, "B1.md", env.acmeToken)
		_ = createFile(t, eA, srcDirID, "C1.bin", env.acmeToken)
		childDirID := createDirectory(t, eA, srcDirID, "SubFolder", env.acmeToken)
		_ = createFile(t, eA, childDirID, "A2.txt", env.acmeToken)
		// add deeper hierarchy
		deepDirID := createDirectory(t, eA, childDirID, "Deep", env.acmeToken)
		_ = createFile(t, eA, deepDirID, "D1.txt", env.acmeToken)

		// Destination directory under the second shared drive
		destDirID := createDirectory(t, eA, secondRootDirID, "DestForFolder"+strings.ReplaceAll(t.Name(), "/", "_"), env.acmeToken)

		// Move the directory subtree
		postMove(t, eB, env.bettyToken, `{
				  "source": {
				    "instance": "https://`+env.acme.Domain+`",
				    "sharing_id": "`+env.firstSharingID+`",
				    "dir_id": "`+srcDirID+`"
				  },
				  "dest": {
				    "instance": "https://`+env.acme.Domain+`",
				    "sharing_id": "`+secondSharingID+`",
				    "dir_id": "`+destDirID+`"	
				  }
				}`)

		// Verify using VFS on owner instance (IDs are not preserved; check by names and paths)
		// 1) Destination now contains a directory named like the source root
		destRoot, err := env.acme.VFS().DirByID(destDirID)
		require.NoError(t, err)
		movedRoot, err := env.acme.VFS().DirByPath(destRoot.Fullpath + "/" + dirToMoveName)
		require.NoError(t, err)
		require.Equal(t, dirToMoveName, movedRoot.DocName)

		// 2) Moved root contains multiple files and subdir SubFolder
		_, err = env.acme.VFS().FileByPath(movedRoot.Fullpath + "/A1.txt")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(movedRoot.Fullpath + "/B1.md")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(movedRoot.Fullpath + "/C1.bin")
		require.NoError(t, err)
		childDir, err := env.acme.VFS().DirByPath(movedRoot.Fullpath + "/SubFolder")
		require.NoError(t, err)

		// 3) Child dir contains A2.txt and nested Deep/D1.txt
		_, err = env.acme.VFS().FileByPath(childDir.Fullpath + "/A2.txt")
		require.NoError(t, err)
		deepDir, err := env.acme.VFS().DirByPath(childDir.Fullpath + "/Deep")
		require.NoError(t, err)
		_, err = env.acme.VFS().FileByPath(deepDir.Fullpath + "/D1.txt")
		require.NoError(t, err)

		// 4) Original source path no longer exists
		productRoot, err := env.acme.VFS().DirByID(env.productDirID)
		require.NoError(t, err)
		_, err = env.acme.VFS().DirByPath(productRoot.Fullpath + "/" + dirToMoveName)
		require.Error(t, err)
	})

	// Validation errors for Move endpoint
	t.Run("BadRequest_MissingSourceFileID", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		// missing source.file_id
		_ = postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {},
			  "dest": {"dir_id": "`+env.productDirID+`"}
		}`, 400)
	})

	t.Run("BadRequest_MissingDestDirID", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		fileID := createFile(t, eB, "", "file-missing-dest.txt", env.bettyToken)
		_ = postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {"file_id": "`+fileID+`"},
			  "dest": {}
		}`, 400)
	})

	t.Run("BadRequest_SourceInstanceWithoutSharingID", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		fileID := createFile(t, eB, "", "file-src-no-share.txt", env.bettyToken)
		_ = postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {"instance": "https://`+env.acme.Domain+`", "file_id": "`+fileID+`"},
			  "dest": {"dir_id": "`+env.productDirID+`"}
		}`, 400)
	})

	t.Run("BadRequest_DestInstanceWithoutSharingID", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		fileID := createFile(t, eB, "", "file-dest-no-share.txt", env.bettyToken)
		_ = postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {"file_id": "`+fileID+`"},
			  "dest": {"instance": "https://`+env.acme.Domain+`", "dir_id": "`+env.productDirID+`"}
		}`, 400)
	})

	t.Run("BadRequest_NoSharingIDsProvided", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		fileID := createFile(t, eB, "", "file-no-shares.txt", env.bettyToken)
		_ = postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {"file_id": "`+fileID+`"},
			  "dest": {"dir_id": "`+env.productDirID+`"}
		}`, 400)
	})

	t.Run("BadRequest_InvalidSourceInstanceURL", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		fileID := createFile(t, eB, "", "file-bad-src-url.txt", env.bettyToken)
		_ = postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {"instance": "not-a-url", "file_id": "`+fileID+`", "sharing_id": "`+env.firstSharingID+`"},
			  "dest": {"dir_id": "`+env.productDirID+`"}
		}`, 400)
	})

	t.Run("BadRequest_InvalidDestInstanceURL", func(t *testing.T) {
		_, eB, _ := env.createClients(t)
		fileID := createFile(t, eB, "", "file-bad-dst-url.txt", env.bettyToken)
		_ = postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {"file_id": "`+fileID+`"},
			  "dest": {"instance": "://broken", "dir_id": "`+env.productDirID+`", "sharing_id": "`+env.firstSharingID+`"}
		}`, 400)
	})

	t.Run("Unprocessable_MoveFromFileRootSharedDrive", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		rootFileName := testify(t, "file-root-root.txt")
		rootFileID := createFile(t, eA, "", rootFileName, env.acmeToken)
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			testify(t, "File-root move source"),
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

		destDirID := createRootDirectory(t, eB, testify(t, "Betty move dest"), env.bettyToken)

		postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+sharingID+`",
			    "file_id": "`+rootFileID+`"
			  },
			  "dest": {
			    "dir_id": "`+destDirID+`"
			  }
			}`, 422)
	})

	t.Run("Unprocessable_MoveToFileRootSharedDrive", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		sourceFileID := createFile(t, eB, "", testify(t, "local-move-to-file-drive.txt"), env.bettyToken)
		destRootFileID := createFile(t, eA, "", testify(t, "file-root-dest-root.txt"), env.acmeToken)
		sharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			destRootFileID,
			testify(t, "File-root move dest"),
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sharingID)

		postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {
			    "file_id": "`+sourceFileID+`"
			  },
			  "dest": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+sharingID+`",
			    "dir_id": "`+destRootFileID+`"
			  }
			}`, 422)
	})

	// Dave is a read-only member; he must not be able to move files to a shared drive
	t.Run("PermissionDeniedWithoutShare_ToSharedDrive", func(t *testing.T) {
		_, _, eD := env.createClients(t)
		// Dave creates a file on his own instance
		fileOnDave := createFile(t, eD, "", "dave-local.txt", env.daveToken)
		// Try to move Dave's file into the shared drive → forbidden
		_ = postMoveExpectStatus(t, eD, env.daveToken, `{
			  "source": { "file_id": "`+fileOnDave+`" },
			  "dest": {"instance": "https://`+env.acme.Domain+`", "sharing_id": "`+env.firstSharingID+`", "dir_id": "`+env.productDirID+`"}
		}`, 403)
	})

	t.Run("PermissionDeniedReadonly_ToSharedDrive", func(t *testing.T) {
		_, _, eD := env.createClients(t)
		// Prepare: create a second shared drive with Dave as read-only recipient
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			testify(t, "SharedDrive"), "Drive used as destination for nested dir move", nil)

		// Dave needs to accept the sharing invitation (read-only recipients still need to accept)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, secondSharingID)

		// Dave should be able to access the shared drive as a read-only member
		// Let's verify Dave can access the shared drive first
		eD.GET("/sharings/drives/"+secondSharingID+"/"+secondRootDirID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		fileToMoveSameStack := createFile(t, eD, "", testify(t, "file-to-upload.txt"), env.daveToken)
		// Dave should get a permission denied error when trying to move files to the shared drive
		// because he's a read-only member
		postMoveExpectStatus(t, eD, env.daveToken, `{
				  "source": {
				    "file_id": "`+fileToMoveSameStack+`"
				  },
				  "dest": {
				    "instance": "https://`+env.acme.Domain+`",
				    "sharing_id": "`+secondSharingID+`",
				    "dir_id": "`+secondRootDirID+`"
				  }
				}`, 403)
	})

	t.Run("PermissionDeniedReadonly_ToSharedDrive_DifferentStack", func(t *testing.T) {
		_, _, eD := env.createClients(t)
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		// Prepare: create a second shared drive with Dave as read-only recipient
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"ShareDrive"+strings.ReplaceAll(t.Name(), "/", "_"), "Drive used as destination for nested dir move", nil)

		// Dave needs to accept the sharing invitation (read-only recipients still need to accept)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, secondSharingID)

		eD.GET("/sharings/drives/"+secondSharingID+"/"+secondRootDirID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		fileToMoveSameStack := createFile(t, eD, "", "file-to-upload.txt", env.daveToken)
		postMoveExpectStatus(t, eD, env.daveToken, `{
				  "source": {
				    "file_id": "`+fileToMoveSameStack+`"
				  },
				  "dest": {
				    "instance": "https://`+env.acme.Domain+`",
				    "sharing_id": "`+secondSharingID+`",
				    "dir_id": "`+secondRootDirID+`"
				  }
				}`, 403)
	})

	t.Run("PermissionDeniedReadonly_FromSharedDrive_SameStack", func(t *testing.T) {
		eA, _, eD := env.createClients(t)
		// Prepare: create a second shared drive with Dave as read-only recipient
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"ShareDrive"+strings.ReplaceAll(t.Name(), "/", "_"), "Drive used as destination for nested dir move", nil)
		fileToMoveSameStack := createFile(t, eA, "", "file-to-upload.txt", env.acmeToken)
		daveDirID := createDirectory(t, eD, "", "DaveDir", env.daveToken)

		// Dave needs to accept the sharing invitation (read-only recipients still need to accept)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, secondSharingID)

		eD.GET("/sharings/drives/"+secondSharingID+"/"+secondRootDirID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		postMoveExpectStatus(t, eD, env.daveToken, `{
				  "source": {
				    "file_id": "`+fileToMoveSameStack+`",
					"sharing_id": "`+secondSharingID+`",
					"instance": "https://`+env.acme.Domain+`"
				  },
				  "dest": {
				    "dir_id": "`+daveDirID+`"
				  }
				}`, 403)
	})

	t.Run("PermissionDeniedReadonly_FromSharedDrive_DifferentStack", func(t *testing.T) {
		eA, _, eD := env.createClients(t)
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()
		// Prepare: create a second shared drive with Dave as read-only recipient
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			testify(t, "ShareDrive"), "Drive used as destination for nested dir move", nil)
		fileToMoveSameStack := createFile(t, eA, "", testify(t, "file-to-upload.txt"), env.acmeToken)
		daveDirID := createDirectory(t, eD, "", testify(t, "DaveDir"), env.daveToken)

		// Dave needs to accept the sharing invitation (read-only recipients still need to accept)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, secondSharingID)

		eD.GET("/sharings/drives/"+secondSharingID+"/"+secondRootDirID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		postMoveExpectStatus(t, eD, env.daveToken, `{
				  "source": {
				    "file_id": "`+fileToMoveSameStack+`",
					"sharing_id": "`+secondSharingID+`",
					"instance": "https://`+env.acme.Domain+`"
				  },
				  "dest": {
				    "dir_id": "`+daveDirID+`"
				  }
				}`, 403)
	})

	// Dave is a read-only member; he must not be able to move files out of a shared drive
	t.Run("PermissionDeniedWithoutShare_FromSharedDrive", func(t *testing.T) {
		eA, _, eD := env.createClients(t)
		// ACME (owner) creates a file in the shared drive that Dave can see
		sharedFile := createFile(t, eA, env.meetingsDirID, "dave-cannot-move.txt", env.acmeToken)
		// Dave creates a destination directory on his instance
		daveDestDir := createRootDirectory(t, eD, "Dave Dest Dir", env.daveToken)
		// Try to move the shared file to Dave's instance → forbidden
		_ = postMoveExpectStatus(t, eD, env.daveToken, `{
			  "source": {"instance": "https://`+env.acme.Domain+`", "sharing_id": "`+env.firstSharingID+`", "file_id": "`+sharedFile+`"},
			  "dest": {"dir_id": "`+daveDestDir+`"}
		}`, 403)
	})

	// Dave is a read-only member; he must not be able to delete files from a shared drive
	t.Run("PermissionDeniedReadonly_DeleteFileFromSharedDrive", func(t *testing.T) {
		eA, _, eD := env.createClients(t)
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		// Prepare: create a second shared drive with Dave as read-only recipient
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			testify(t, "ShareDrive"), "Drive used for testing delete permissions", nil)

		// Create a file in the shared drive (by Acme)
		fileToDeleteID := createFile(t, eA, secondRootDirID, testify(t, "file-to-delete.txt"), env.acmeToken)

		// Dave needs to accept the sharing invitation (read-only recipients still need to accept)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, secondSharingID)

		// Verify Dave can access the shared drive
		eD.GET("/sharings/drives/"+secondSharingID+"/"+secondRootDirID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		// Verify Dave can access the file through the shared drive endpoint
		eD.GET("/sharings/drives/"+secondSharingID+"/"+fileToDeleteID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		// Try to delete the file → should be forbidden (403)
		eD.DELETE("/sharings/drives/"+secondSharingID+"/"+fileToDeleteID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(403)

		// Verify the file still exists (was not deleted)
		verifyFileExists(t, env.acme, fileToDeleteID, testify(t, "file-to-delete.txt"), secondRootDirID, "foo")
	})
}

func TestSharedDrivesCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)

	// Test 1: Copy file between shared drives, same stack
	t.Run("SuccessfulCopy_FileBetweenSharedDrives_SameStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Create a second shared drive
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"SecondDrive", "Second shared drive for copy tests", nil)
		// Accept it as Betty
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Create a file in the first shared drive
		fileToCopy := createFile(t, eA, env.meetingsDirID, "file-to-copy.txt", env.acmeToken)

		// Copy the file to the second shared drive
		responseObj := postMove(t, eB, env.bettyToken, `{
			"source": {
				"instance": "https://`+env.acme.Domain+`",
				"sharing_id": "`+env.firstSharingID+`",
				"file_id": "`+fileToCopy+`"
			},
			"dest": {
				"instance": "https://`+env.acme.Domain+`",
				"sharing_id": "`+secondSharingID+`",
				"dir_id": "`+secondRootDirID+`"
			},
			"copy": true
		}`)

		// Verify the response and get copied file ID
		copiedFileID := assertMoveResponseWithSharing(t, responseObj, "file-to-copy.txt", secondRootDirID, secondSharingID)

		// Verify the file was copied and content preserved
		verifyFileMove(t, env.acme, copiedFileID, "file-to-copy.txt", secondRootDirID, "foo")

		// Verify the original file still exists (not deleted)
		verifyFileExists(t, env.acme, fileToCopy, "file-to-copy.txt", env.meetingsDirID, "foo")
	})

	t.Run("SuccessfulCopy_FileFromFileRootSharedDriveToDirectoryBackedSharedDrive_SameStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		fileName := testify(t, "file-root-to-shared.txt")
		rootFileID := createFile(t, eA, "", fileName, env.acmeToken)
		sourceSharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			testify(t, "File-root copy source"),
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sourceSharingID)

		responseObj := postMove(t, eB, env.bettyToken, `{
			"source": {
				"instance": "https://`+env.acme.Domain+`",
				"sharing_id": "`+sourceSharingID+`",
				"file_id": "`+rootFileID+`"
			},
			"dest": {
				"instance": "https://`+env.acme.Domain+`",
				"sharing_id": "`+env.firstSharingID+`",
				"dir_id": "`+env.productDirID+`"
			},
			"copy": true
		}`)

		copiedFileID := assertMoveResponseWithSharing(t, responseObj, fileName, env.productDirID, env.firstSharingID)
		verifyFileMove(t, env.acme, copiedFileID, fileName, env.productDirID, "foo")
		verifyFileExists(t, env.acme, rootFileID, fileName, "", "foo")
	})

	// Test 2: Copy directory between shared drives, same stack
	t.Run("SuccessfulCopy_DirectoryBetweenSharedDrives_SameStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Create a second shared drive
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			"ThirdDrive", "Third shared drive for copy tests", nil)
		// Accept it as Betty
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, secondSharingID)

		// Create a directory with files in the first shared drive
		dirToCopy := createDirectory(t, eA, env.meetingsDirID, "DirToCopy", env.acmeToken)
		_ = createFile(t, eA, dirToCopy, "file1.txt", env.acmeToken)
		_ = createFile(t, eA, dirToCopy, "file2.md", env.acmeToken)
		subDirID := createDirectory(t, eA, dirToCopy, "SubDir", env.acmeToken)
		_ = createFile(t, eA, subDirID, "file3.bin", env.acmeToken)

		// Copy the directory to the second shared drive
		responseObj := postMove(t, eB, env.bettyToken, `{
			"source": {
				"instance": "https://`+env.acme.Domain+`",
				"sharing_id": "`+env.firstSharingID+`",
				"dir_id": "`+dirToCopy+`"
			},
			"dest": {
				"instance": "https://`+env.acme.Domain+`",
				"sharing_id": "`+secondSharingID+`",
				"dir_id": "`+secondRootDirID+`"
			},
			"copy": true
		}`)

		// Verify the response and get copied directory ID
		copiedDirID := assertDirectoryResponse(t, responseObj, "DirToCopy", secondRootDirID)

		// Verify the directory was copied with all its contents
		verifyDirectoryCopy(t, env.acme, copiedDirID, "DirToCopy", secondRootDirID)

		// Verify the original directory still exists (not deleted)
		verifyDirectoryExists(t, env.acme, dirToCopy, "DirToCopy", env.meetingsDirID)
	})

	// Test 3: Copy file from local to shared drive, different stack
	t.Run("SuccessfulCopy_FileFromLocalToSharedDrive_DifferentStack", func(t *testing.T) {
		_, eB, _ := env.createClients(t)

		// Create a file in Betty's local drive
		fileToCopy := createFile(t, eB, "", "local-file-to-copy.txt", env.bettyToken)

		// Force cross-stack behavior
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		// Copy the file to the shared drive
		responseObj := postMove(t, eB, env.bettyToken, `{
			"source": {
				"file_id": "`+fileToCopy+`"
			},
			"dest": {
				"instance": "https://`+env.acme.Domain+`",
				"sharing_id": "`+env.firstSharingID+`",
				"dir_id": "`+env.productDirID+`"
			},
			"copy": true
		}`)

		// Verify the response and get copied file ID
		copiedFileID := assertMoveResponseWithSharing(t, responseObj, "local-file-to-copy.txt", env.productDirID, env.firstSharingID)

		// Verify the file was copied and content preserved
		verifyFileMove(t, env.acme, copiedFileID, "local-file-to-copy.txt", env.productDirID, "foo")

		// Verify the original file still exists (not deleted)
		verifyFileExists(t, env.betty, fileToCopy, "local-file-to-copy.txt", "", "foo")
	})

	// Test 4: Copy directory from shared drive to local, different stack
	t.Run("SuccessfulCopy_DirectoryFromSharedDriveToLocal_DifferentStack", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		// Create a directory with files in the shared drive
		dirToCopy := createDirectory(t, eA, env.meetingsDirID, "SharedDirToCopy", env.acmeToken)
		_ = createFile(t, eA, dirToCopy, "shared-file1.txt", env.acmeToken)
		_ = createFile(t, eA, dirToCopy, "shared-file2.md", env.acmeToken)
		subDirID := createDirectory(t, eA, dirToCopy, "SharedSubDir", env.acmeToken)
		_ = createFile(t, eA, subDirID, "shared-file3.bin", env.acmeToken)

		// Create destination directory in Betty's local drive
		destDirID := createRootDirectory(t, eB, "LocalDestDir", env.bettyToken)

		// Force cross-stack behavior
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		// Copy the directory to Betty's local drive
		responseObj := postMove(t, eB, env.bettyToken, `{
			"source": {
				"instance": "https://`+env.acme.Domain+`",
				"sharing_id": "`+env.firstSharingID+`",
				"dir_id": "`+dirToCopy+`"
			},
			"dest": {
				"dir_id": "`+destDirID+`"
			},
			"copy": true
		}`)

		// Verify the response and get copied directory ID
		copiedDirID := assertDirectoryResponse(t, responseObj, "SharedDirToCopy", destDirID)

		// Verify the directory was copied with all its contents
		verifyDirectoryCopy(t, env.betty, copiedDirID, "SharedDirToCopy", destDirID)

		// Verify the original directory still exists (not deleted)
		verifyDirectoryExists(t, env.acme, dirToCopy, "SharedDirToCopy", env.meetingsDirID)
	})

	t.Run("SuccessfulCopy_FileFromSharedDriveToLocal_Readonly_DifferentStack", func(t *testing.T) {
		eA, _, eD := env.createClients(t)
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		// Prepare: create a second shared drive with Dave as read-only recipient
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			testify(t, "ShareDrive"), "Drive used as destination for nested dir move", nil)
		// Dave needs to accept the sharing invitation (read-only recipients still need to accept)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, secondSharingID)

		fileToMoveName := testify(t, "file-to-upload.txt")
		fileToMoveID := createFile(t, eA, secondRootDirID, fileToMoveName, env.acmeToken)

		daveDirID := createDirectory(t, eD, "", testify(t, "DaveDir"), env.daveToken)

		// Verify Dave can access the shared drive
		eD.GET("/sharings/drives/"+secondSharingID+"/"+secondRootDirID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		// Verify Dave can access the file through the shared drive endpoint
		eD.GET("/sharings/drives/"+secondSharingID+"/"+fileToMoveID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		responseObj := postMove(t, eD, env.daveToken, `{
				  "source": {
				    "file_id": "`+fileToMoveID+`",
					"sharing_id": "`+secondSharingID+`",
					"instance": "https://`+env.acme.Domain+`"
				  },
				  "dest": {
				    "dir_id": "`+daveDirID+`"
				  },
				  "copy": true
				}`)

		// Verify the response and get copied file ID
		copiedFileID := assertMoveResponse(t, responseObj, fileToMoveName, daveDirID)

		// Verify the file was copied and content preserved
		verifyFileMove(t, env.dave, copiedFileID, fileToMoveName, daveDirID, "foo")

		// Verify the original directory still exists (not deleted)
		verifyFileExists(t, env.acme, fileToMoveID, fileToMoveName, secondRootDirID, "foo")
	})

	t.Run("SuccessfulCopy_FileFromFileRootSharedDriveToLocal_Readonly_DifferentStack", func(t *testing.T) {
		eA, _, eD := env.createClients(t)
		cleanup := forceCrossStack(t, env.tsA.URL)
		defer cleanup()

		fileName := testify(t, "file-root-readonly-copy.txt")
		rootFileID := createFile(t, eA, "", fileName, env.acmeToken)
		sourceSharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			testify(t, "File-root readonly copy source"),
			[]RecipientInfo{{Name: "Dave", Email: "dave@example.net", ReadOnly: true}},
		)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, sourceSharingID)

		daveDirID := createDirectory(t, eD, "", testify(t, "DaveFileBackedDir"), env.daveToken)

		responseObj := postMove(t, eD, env.daveToken, `{
			  "source": {
			    "file_id": "`+rootFileID+`",
				"sharing_id": "`+sourceSharingID+`",
				"instance": "https://`+env.acme.Domain+`"
			  },
			  "dest": {
			    "dir_id": "`+daveDirID+`"
			  },
			  "copy": true
			}`)

		copiedFileID := assertMoveResponse(t, responseObj, fileName, daveDirID)
		verifyFileMove(t, env.dave, copiedFileID, fileName, daveDirID, "foo")
		verifyFileExists(t, env.acme, rootFileID, fileName, "", "foo")
	})

	t.Run("SuccessfulCopy_FileFromSharedDriveToLocal_Readonly_SameStack", func(t *testing.T) {
		eA, _, eD := env.createClients(t)

		// Prepare: create a second shared drive with Dave as read-only recipient
		secondSharingID, secondRootDirID, _ := createSharedDrive(t, DriveCreationMethodLegacy, env.acme, env.acmeToken, env.tsA.URL,
			testify(t, "ShareDrive"), "Drive used as destination for nested dir move", nil)
		// Dave needs to accept the sharing invitation (read-only recipients still need to accept)
		acceptSharedDrive(t, env.acme, env.dave, "Dave", env.tsA.URL, env.tsD.URL, secondSharingID)

		fileToMoveName := testify(t, "file-to-upload.txt")
		fileToMoveID := createFile(t, eA, secondRootDirID, fileToMoveName, env.acmeToken)

		daveDirID := createDirectory(t, eD, "", testify(t, "DaveDir"), env.daveToken)

		// Verify Dave can access the shared drive
		eD.GET("/sharings/drives/"+secondSharingID+"/"+secondRootDirID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		// Verify Dave can access the file through the shared drive endpoint
		eD.GET("/sharings/drives/"+secondSharingID+"/"+fileToMoveID).
			WithHeader("Authorization", "Bearer "+env.daveToken).
			Expect().Status(200)

		responseObj := postMove(t, eD, env.daveToken, `{
				  "source": {
				    "file_id": "`+fileToMoveID+`",
					"sharing_id": "`+secondSharingID+`",
					"instance": "https://`+env.acme.Domain+`"
				  },
				  "dest": {
				    "dir_id": "`+daveDirID+`"
				  },
				  "copy": true
				}`)

		// Verify the response and get copied file ID
		copiedFileID := assertMoveResponse(t, responseObj, fileToMoveName, daveDirID)

		// Verify the file was copied and content preserved
		verifyFileMove(t, env.dave, copiedFileID, fileToMoveName, daveDirID, "foo")

		// Verify the original directory still exists (not deleted)
		verifyFileExists(t, env.acme, fileToMoveID, fileToMoveName, secondRootDirID, "foo")
	})

	t.Run("Unprocessable_CopyFromFileRootSharedDriveWithNonRootFile", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		rootFileID := createFile(t, eA, "", testify(t, "file-root-root.txt"), env.acmeToken)
		otherFileID := createFile(t, eA, "", testify(t, "file-root-outside.txt"), env.acmeToken)
		sourceSharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			rootFileID,
			testify(t, "File-root non-root copy source"),
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, sourceSharingID)

		destDirID := createRootDirectory(t, eB, testify(t, "Betty copy dest"), env.bettyToken)

		postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+sourceSharingID+`",
			    "file_id": "`+otherFileID+`"
			  },
			  "dest": {
			    "dir_id": "`+destDirID+`"
			  },
			  "copy": true
			}`, 422)
	})

	t.Run("Unprocessable_CopyToFileRootSharedDrive", func(t *testing.T) {
		eA, eB, _ := env.createClients(t)

		sourceFileID := createFile(t, eB, "", testify(t, "local-copy-to-file-drive.txt"), env.bettyToken)
		destRootFileID := createFile(t, eA, "", testify(t, "file-root-copy-dest-root.txt"), env.acmeToken)
		destSharingID, _ := createFileRootSharedDrive(
			t,
			env.acme,
			env.acmeToken,
			env.tsA.URL,
			destRootFileID,
			testify(t, "File-root copy dest"),
			[]RecipientInfo{{Name: "Betty", Email: "betty@example.net", ReadOnly: false}},
		)
		acceptSharedDriveForBetty(t, env.acme, env.betty, env.tsA.URL, env.tsB.URL, destSharingID)

		postMoveExpectStatus(t, eB, env.bettyToken, `{
			  "source": {
			    "file_id": "`+sourceFileID+`"
			  },
			  "dest": {
			    "instance": "https://`+env.acme.Domain+`",
			    "sharing_id": "`+destSharingID+`",
			    "dir_id": "`+destRootFileID+`"
			  },
			  "copy": true
			}`, 422)
	})
}

func testify(t *testing.T, s string) string {
	return strings.ReplaceAll(t.Name()+"_"+s, "/", "_")
}

// Simple verification helpers to reduce repetitive assertions
func assertMoveResponse(t *testing.T, response *httpexpect.Object, expectedName, expectedDirID string) string {
	t.Helper()
	response.Path("$.data.type").String().IsEqual("io.cozy.files")
	response.Path("$.data.attributes.name").String().IsEqual(expectedName)
	response.Path("$.data.attributes.dir_id").String().IsEqual(expectedDirID)
	return response.Path("$.data.id").String().Raw()
}

func assertMoveResponseWithSharing(t *testing.T, response *httpexpect.Object, expectedName, expectedDirID, expectedSharingID string) string {
	t.Helper()
	response.Path("$.data.type").String().IsEqual("io.cozy.files")
	response.Path("$.data.attributes.name").String().IsEqual(expectedName)
	response.Path("$.data.attributes.dir_id").String().IsEqual(expectedDirID)
	response.Path("$.data.attributes.driveId").String().IsEqual(expectedSharingID)
	return response.Path("$.data.id").String().Raw()
}

func assertDirectoryResponse(t *testing.T, response *httpexpect.Object, expectedName, expectedDirID string) string {
	t.Helper()
	response.Path("$.data.type").String().IsEqual("io.cozy.files")
	response.Path("$.data.attributes.name").String().IsEqual(expectedName)
	response.Path("$.data.attributes.dir_id").String().IsEqual(expectedDirID)
	response.Path("$.data.attributes.type").String().IsEqual("directory")
	return response.Path("$.data.id").String().Raw()
}

func assertAutoRename(t *testing.T, response *httpexpect.Object, originalName string) string {
	t.Helper()
	movedName := response.Path("$.data.attributes.name").String().Raw()
	require.NotEqual(t, originalName, movedName)
	require.Contains(t, movedName, " (2)")
	return movedName
}

func verifyBothFilesExist(t *testing.T, instance *instance.Instance, dirID, originalName, renamedName string) {
	t.Helper()
	meetingsDir, err := instance.VFS().DirByID(dirID)
	require.NoError(t, err)

	// Check original exists (could be file or directory)
	_, err = instance.VFS().FileByPath(meetingsDir.Fullpath + "/" + originalName)
	if err != nil {
		_, err = instance.VFS().DirByPath(meetingsDir.Fullpath + "/" + originalName)
	}
	require.NoError(t, err)

	// Check renamed exists (could be file or directory)
	_, err = instance.VFS().FileByPath(meetingsDir.Fullpath + "/" + renamedName)
	if err != nil {
		_, err = instance.VFS().DirByPath(meetingsDir.Fullpath + "/" + renamedName)
	}
	require.NoError(t, err)
}

// verifyFileExists verifies that a file exists with the expected attributes
func verifyFileExists(t *testing.T, inst *instance.Instance, fileID, expectedName, expectedDirID, expectedContent string) {
	t.Helper()
	fs := inst.VFS()

	fileDoc, err := fs.FileByID(fileID)
	require.NoError(t, err)
	require.Equal(t, expectedName, fileDoc.DocName)
	if expectedDirID != "" {
		require.Equal(t, expectedDirID, fileDoc.DirID)
	}
	require.Equal(t, int64(len(expectedContent)), fileDoc.ByteSize)
}

// verifyDirectoryExists verifies that a directory exists with the expected attributes
func verifyDirectoryExists(t *testing.T, inst *instance.Instance, dirID, expectedName, expectedDirID string) {
	t.Helper()
	fs := inst.VFS()

	dirDoc, err := fs.DirByID(dirID)
	require.NoError(t, err)
	require.Equal(t, expectedName, dirDoc.DocName)
	if expectedDirID != "" {
		require.Equal(t, expectedDirID, dirDoc.DirID)
	}
}

// verifyDirectoryCopy verifies that a directory was copied with all its contents
func verifyDirectoryCopy(t *testing.T, inst *instance.Instance, dirID, expectedName, expectedParentDirID string) {
	t.Helper()
	fs := inst.VFS()

	// Verify the directory exists
	dirDoc, err := fs.DirByID(dirID)
	require.NoError(t, err)
	require.Equal(t, expectedName, dirDoc.DocName)
	require.Equal(t, expectedParentDirID, dirDoc.DirID)

	// Verify the directory is not empty (has contents)
	isEmpty, err := dirDoc.IsEmpty(fs)
	require.NoError(t, err)
	require.False(t, isEmpty, "Copied directory should not be empty")
}
