package sharings_test

import (
	"testing"

	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
