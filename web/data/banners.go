package data

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func updateBanner(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	docid := c.Get("docid").(string)

	var doc couchdb.JSONDoc
	if err := json.NewDecoder(c.Request().Body).Decode(&doc); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}
	if doc.ID() != docid || doc.Rev() == "" {
		return jsonapi.Errorf(http.StatusBadRequest, "a banner update needs the _id and _rev of the stored document")
	}

	var stored couchdb.JSONDoc
	if err := couchdb.GetDoc(instance, consts.Banners, docid, &stored); err != nil {
		return fixErrorNoDatabaseIsWrongDoctype(err)
	}
	stored.Type = consts.Banners
	if err := middlewares.Allow(c, permission.PUT, &stored); err != nil {
		return err
	}

	switch {
	case doc.M["dismissedAt"] == nil:
		stored.M["dismissedAt"] = nil
	case stored.M["dismissible"] != true:
		return jsonapi.Errorf(http.StatusForbidden, "banner %s is not dismissible", docid)
	case stored.M["dismissedAt"] == nil:
		// A client value could be unparsable and break later materializations.
		stored.M["dismissedAt"] = time.Now().UTC()
	}
	stored.SetRev(doc.Rev())

	if err := couchdb.UpdateDoc(instance, &stored); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, echo.Map{
		"ok":   true,
		"id":   stored.ID(),
		"rev":  stored.Rev(),
		"type": stored.DocType(),
		"data": stored.ToMapWithType(),
	})
}
