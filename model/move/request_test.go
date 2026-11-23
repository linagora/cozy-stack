package move

import (
	"testing"

	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestSourceCodeIsSingleUse(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	access, err := oauth.CreateAccessCode(inst, &oauth.Client{ClientID: SourceClientID}, consts.ExportsRequests, "")
	require.NoError(t, err)

	require.NoError(t, checkSourceCode(inst, access.Code))
	require.ErrorIs(t, checkSourceCode(inst, access.Code), permission.ErrInvalidToken)
}
