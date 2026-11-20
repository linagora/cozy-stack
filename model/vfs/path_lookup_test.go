package vfs_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/stretchr/testify/assert"
)

func TestResolvePathUnderRoot(t *testing.T) {
	for _, tc := range []struct{ root, clientPath, want string }{
		{"/Projects/Team", "/Team", "/Projects/Team"},
		{"/Projects/Team", "/Team/drafts/a.txt", "/Projects/Team/drafts/a.txt"},
		{"/Projects/Team", "/Team/drafts/../a.txt", "/Projects/Team/a.txt"},
		{"/Projects/Team", "/Teamwork/a.txt", ""},
		{"/Projects/Team", "/Team/../Other", ""},
		{"/Projects/Team", "Team/a.txt", ""},
		{"", "/Team/a.txt", ""},
		{"/", "/", "/"},
		{"/", "/a.txt", "/a.txt"},
	} {
		assert.Equal(t, tc.want, vfs.ResolvePathUnderRoot(tc.root, tc.clientPath), "%q under %q", tc.clientPath, tc.root)
	}
}
