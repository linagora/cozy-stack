package contacts

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContacts(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	testInstance := setup.GetTestInstance(&lifecycle.Options{
		Email:      "alice@example.com",
		PublicName: "Alice",
	})
	_, token := setup.GetTestClient(consts.Contacts)
	ts := setup.GetTestServer("/contacts", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	t.Run("Myself", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create an empty contact for myself
		e.POST("/contacts/myself").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Get the contact info about myself
		myself, err := contact.GetMyself(testInstance)
		assert.NoError(t, err)

		// Delete the contacts info about myself
		err = couchdb.DeleteDoc(testInstance, myself)
		assert.NoError(t, err)

		// Create again an empty contact for myself ?
		obj := e.POST("/contacts/myself").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Check the contact data
		data := obj.Value("data").Object()
		data.Value("id").String().NotEmpty()
		data.ValueEqual("type", consts.Contacts)

		meta := data.Value("meta").Object()
		meta.Value("rev").String().NotEmpty()

		attrs := data.Value("attributes").Object()
		attrs.ValueEqual("fullname", "Alice")
		emails := attrs.Value("email").Array()
		emails.Length().Equal(1)
		email := emails.First().Object()
		email.ValueEqual("address", "alice@example.com")
		email.ValueEqual("primary", true)
	})
	t.Run("Suggest", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		jean, err := contact.Create(testInstance, contact.CreateOptions{Name: "Jean Dupont", Email: "jean@example.com"})
		require.NoError(t, err)
		suggest := func(q string) *httpexpect.Array {
			return e.GET("/contacts/suggest").WithQuery("q", q).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().Value("data").Array()
		}

		e.GET("/contacts/suggest").WithQuery("q", "du").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(422)
		e.GET("/contacts/suggest").WithQuery("q", "dupont").WithQuery("limit", "0").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(422)

		side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[
				{"names": [{"displayName": "Jean Dupont"}], "emailAddresses": [{"value": "jean@example.com"}]},
				{"names": [{"displayName": "Paul Dupont"}], "emailAddresses": [{"value": "paul@example.com"}]}
			]`))
		}))
		t.Cleanup(side.Close)
		config.GetConfig().ContactsSearch = map[string]config.ContactsSearch{
			testInstance.ContextName: {URL: side.URL, Token: "s3cr3t"},
		}
		t.Cleanup(func() { config.GetConfig().ContactsSearch = nil })
		require.NoError(t, instance.SetEmail(testInstance, fmt.Sprintf("alice-%d@example.com", time.Now().UnixNano())))

		data := suggest("dupont")
		data.Length().Equal(2)
		known := data.Value(0).Object()
		known.ValueEqual("type", consts.Contacts)
		known.ValueEqual("id", jean.ID())
		known.Path("$.meta.kind").Equal("contact")
		known.Path("$.attributes.fullname").Equal("Jean Dupont")
		known.Value("attributes").Object().NotContainsKey("_id").NotContainsKey("_rev")
		// Someone the search knows but no document matches.
		unknown := data.Value(1).Object()
		unknown.NotContainsKey("id")
		unknown.NotContainsKey("meta")
		unknown.Path("$.attributes.fullname").Equal("Paul Dupont")
		unknown.Path("$.attributes.email[0].address").Equal("paul@example.com")
	})
}
