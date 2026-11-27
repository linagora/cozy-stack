package sharing

import (
	"testing"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"

	"github.com/stretchr/testify/assert"
)

func TestDriveRootAtTheInstanceRoot(t *testing.T) {
	s := &Sharing{driveRootPath: "/"}
	assert.Equal(t, "/Docs/a.txt", s.DriveRelativePath("/Docs/a.txt"))
	assert.Equal(t, "/", s.DriveRelativePath("/"))
	assert.Equal(t, "/Docs/a.txt", s.DriveAbsolutePath("/Docs/a.txt"))
	assert.Equal(t, "/Docs", s.DriveAbsolutePath("/Docs/../Docs"))
}

func TestMemberDrivePathRoot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rules  permission.Set
		hidden bool
	}{
		{"drive root", permission.Set{{Type: consts.Files, Values: []string{"drive"}}}, false},
		{"duplicate ID", permission.Set{{Type: consts.Files, Values: []string{"drive", "drive"}}}, false},
		{"different root", permission.Set{{Type: consts.Files, Values: []string{"other"}}}, true},
		{"two roots", permission.Set{{Type: consts.Files, Values: []string{"drive", "other"}}}, true},
		{"whole doctype", permission.Set{{Type: consts.Files}}, true},
		{"field selector", permission.Set{{Type: consts.Files, Selector: "name", Values: []string{"drive"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sharing{Drive: true, Rules: []Rule{{DocType: consts.Files, Values: []string{"drive"}}}}
			s.LimitDrivePaths(&permission.Permission{Type: permission.TypeShareInteract, Permissions: tc.rules})
			assert.Equal(t, tc.hidden, s.DrivePathsHidden())
		})
	}
}

func TestPreviewDrivePathRoot(t *testing.T) {
	s := &Sharing{Drive: true, Rules: []Rule{{DocType: consts.Files, Values: []string{"drive"}}}}
	s.LimitDrivePaths(&permission.Permission{
		Type:        permission.TypeSharePreview,
		Permissions: permission.Set{{Type: consts.Files, Values: []string{"nested"}}},
	})
	if assert.NotNil(t, s.visibleRootID) {
		assert.Equal(t, "nested", *s.visibleRootID)
	}

	// A parent the link can only write to does not count as a root
	s = &Sharing{Drive: true, Rules: []Rule{{DocType: consts.Files, Values: []string{"drive"}}}}
	s.LimitDrivePaths(&permission.Permission{
		Type: permission.TypeShareByLink,
		Permissions: permission.Set{
			{Type: consts.Files, Verbs: permission.Verbs(permission.POST), Values: []string{"drive"}},
			{Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: []string{"nested"}},
		},
	})
	if assert.NotNil(t, s.visibleRootID) {
		assert.Equal(t, "nested", *s.visibleRootID)
	}
}
