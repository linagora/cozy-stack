package space

import (
	"os"
	"testing"

	"github.com/cozy/cozy-stack/tests/testutils"
)

func TestMain(m *testing.M) {
	os.Exit(testutils.RunTestMainWithCouchDB(m))
}
