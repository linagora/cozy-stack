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
	"github.com/cozy/cozy-stack/model/orgdirectory"
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
	_, token := setup.GetTestClient(consts.Contacts + " " + consts.Groups)
	_, contactsOnly := setup.GetTestClient(consts.Contacts)
	_, groupsOnly := setup.GetTestClient(consts.Groups)
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

		e.GET("/contacts/suggest").WithQuery("q", "d").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(422)
		e.GET("/contacts/suggest").WithQuery("q", "dupont").WithQuery("limit", "0").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(422)

		for _, restricted := range []string{contactsOnly, groupsOnly} {
			e.GET("/contacts/suggest").WithQuery("q", "du").
				WithHeader("Authorization", "Bearer "+restricted).Expect().Status(403)
		}
		suggest("du").Length().Equal(1)

		// Without centralized_org_directory, the local databases are searched.
		local := suggest("DUPONT")
		local.Length().Equal(1)
		local.Value(0).Object().ValueEqual("id", jean.ID())

		side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[
				{"names": [{"displayName": "Jean Dupont"}], "emailAddresses": [{"value": "jean@example.com"}]},
				{"names": [{"displayName": "Paul Dupont"}], "emailAddresses": [{"value": "paul@example.com"}]}
			]`))
		}))
		t.Cleanup(side.Close)
		previous := config.GetConfig().Contexts
		config.GetConfig().Contexts = map[string]interface{}{config.DefaultInstanceContext: map[string]interface{}{
			"centralized_org_directory": true, "common_contacts": true,
		}}
		t.Cleanup(func() { config.GetConfig().Contexts = previous })
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

		config.GetConfig().Contexts[config.DefaultInstanceContext] = map[string]interface{}{"centralized_org_directory": true}
		suggest("dupont").Length().Equal(1)

		config.GetConfig().Contexts[config.DefaultInstanceContext] = map[string]interface{}{
			"centralized_org_directory": false, "common_contacts": true,
		}
		group := contact.NewGroup()
		group.SetID("friends")
		group.M["name"] = "Dupont [Friends]"
		require.NoError(t, couchdb.CreateNamedDocWithDB(testInstance, group))
		member := contact.New()
		member.M["relationships"] = map[string]interface{}{
			"groups": map[string]interface{}{"data": []interface{}{
				map[string]interface{}{"_id": group.ID(), "_type": consts.Groups},
			}},
		}
		require.NoError(t, couchdb.CreateDoc(testInstance, member))
		local = suggest("dupont")
		local.Length().Equal(2)
		localGroup := local.First().Object()
		localGroup.ValueEqual("type", consts.Groups)
		localGroup.ValueEqual("id", group.ID())
		localGroup.Path("$.attributes.name").Equal("Dupont [Friends]")
		localGroup.Path("$.attributes.color").Equal("")
		localGroup.Path("$.attributes.member_count").Equal(1)
		local.Value(1).Object().ValueEqual("id", jean.ID())
		suggest("[f").Length().Equal(1)
	})
}

func TestSuggestOrganizationGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	previous := config.GetConfig().Contexts
	config.GetConfig().Contexts = map[string]interface{}{config.DefaultInstanceContext: map[string]interface{}{
		"centralized_org_directory": true,
	}}
	t.Cleanup(func() { config.GetConfig().Contexts = previous })
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "suggest" + suffix
	orgDomain := orgID + ".example"
	setup := testutils.NewSetup(t, t.Name())
	setup.GetTestInstance(&lifecycle.Options{
		Domain: "alice-" + suffix + ".sg.localhost", OrgID: orgID, OrgDomain: orgDomain,
		Email: "alice@example.com",
	})
	org, err := lifecycle.Create(&lifecycle.Options{
		Domain: orgID + ".sg.localhost", OrgID: orgID, OrgDomain: orgDomain,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(org.Domain) })
	group := contact.NewGroup()
	group.SetID(orgdirectory.GroupDocID(orgID, "it"))
	group.M["name"] = "IT"
	group.M["color"] = "#123456"
	group.M[orgdirectory.DirectoryMetadataKey] = map[string]interface{}{"managed": true, "organizationId": orgID}
	require.NoError(t, couchdb.CreateNamedDocWithDB(org, group))
	_, token := setup.GetTestClient(consts.Contacts + " " + consts.Groups)
	ts := setup.GetTestServer("/contacts", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)
	e := testutils.CreateTestClient(t, ts.URL)
	data := e.GET("/contacts/suggest").WithQuery("q", "it").
		WithHeader("Authorization", "Bearer "+token).Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object().Value("data").Array()
	data.Length().Equal(1)
	result := data.First().Object()
	result.ValueEqual("type", consts.Groups)
	result.ValueEqual("id", group.ID())
	result.Path("$.attributes.name").Equal("IT")
	result.Path("$.attributes.color").Equal("#123456")
	result.Path("$.attributes.member_count").Equal(0)
	result.Value("attributes").Object().NotContainsKey(orgdirectory.DirectoryMetadataKey)
}
