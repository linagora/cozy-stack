package sharings_test

import (
	"net/http"
	"testing"

	"github.com/gavv/httpexpect/v2"
)

func putFileWithCode(e *httpexpect.Expect, fileID, code string, status int) {
	e.PUT("/files/"+fileID).
		WithHeader("Authorization", "Bearer "+code).
		WithHeader("Content-Type", "text/plain").
		WithBytes([]byte("written with a drive code")).
		Expect().Status(status)
}

func getFileWithCode(e *httpexpect.Expect, fileID, code string, status int) {
	e.GET("/files/"+fileID).
		WithHeader("Authorization", "Bearer "+code).
		Expect().Status(status)
}

func TestSharedDriveInteractCodeOfReadOnlyMemberCannotWrite(t *testing.T) {
	f := setupSharedDrivePermissionFixture(t)
	sharingID, productID := createReadOnlyDriveForDave(t, f)
	fileID := createFile(t, f.eOwner, productID, "read-only-code.txt", f.ownerAppToken)
	code := getSharedDriveInteractCode(t, f.env.acme, sharingID, "dave@example.net")

	getFileWithCode(f.eOwner, fileID, code, http.StatusOK)
	putFileWithCode(f.eOwner, fileID, code, http.StatusForbidden)
}

func TestSharedDriveInteractCodeFollowsTheRoleOfTheMember(t *testing.T) {
	f := setupSharedDrivePermissionFixture(t)
	code := getSharedDriveInteractCode(t, f.env.acme, f.sharingID, "betty@example.net")
	putFileWithCode(f.eOwner, f.fileID, code, http.StatusOK)

	f.eOwner.POST("/sharings/"+f.sharingID+"/recipients/1/readonly").
		WithHeader("Authorization", "Bearer "+f.ownerAppToken).
		Expect().Status(http.StatusNoContent)
	waitForSharingMemberReadOnly(t, f.env.acme, f.sharingID, "betty@example.net", true)

	getFileWithCode(f.eOwner, f.fileID, code, http.StatusOK)
	putFileWithCode(f.eOwner, f.fileID, code, http.StatusForbidden)

	f.eOwner.DELETE("/sharings/"+f.sharingID+"/recipients/1/readonly").
		WithHeader("Authorization", "Bearer "+f.ownerAppToken).
		Expect().Status(http.StatusNoContent)
	waitForSharingMemberReadOnly(t, f.env.acme, f.sharingID, "betty@example.net", false)

	putFileWithCode(f.eOwner, f.fileID, code, http.StatusOK)
}
