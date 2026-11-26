package permission

import (
	"testing"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/assert"
)

func TestCheckWritableRemoteDoctypes(t *testing.T) {
	assert.Error(t, CheckWritable(consts.Doctypes))
	assert.NoError(t, CheckReadable(consts.Doctypes))
}
