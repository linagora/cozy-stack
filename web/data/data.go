// Package data provide simple CRUD operation on couchdb doc
package data

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/stream"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func paramIsTrue(c echo.Context, param string) bool {
	return c.QueryParam(param) == "true"
}

// ValidDoctype validates the doctype and sets it in the context of the request.
func ValidDoctype(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		doctype := c.Param("doctype")
		if doctype == "" {
			return jsonapi.Errorf(http.StatusBadRequest, "Invalid doctype '%s'", doctype)
		}

		docidraw := c.Param("docid")
		docid, err := url.QueryUnescape(docidraw)
		if err != nil {
			return jsonapi.Errorf(http.StatusBadRequest, "Invalid docid '%s'", docid)
		}
		c.Set("docid", docid)

		return next(c)
	}
}

func fixErrorNoDatabaseIsWrongDoctype(err error) error {
	if couchdb.IsNoDatabaseError(err) {
		err.(*couchdb.Error).Reason = "wrong_doctype"
	}
	return err
}

const managedDirectoryWriteReason = "managed organization directory documents are read-only"

func managedDirectoryWriteError() error {
	return jsonapi.Errorf(http.StatusForbidden, managedDirectoryWriteReason)
}

func rejectManagedDirectoryCreate(doc *couchdb.JSONDoc) error {
	if orgdirectory.IsManagedDirectoryDoctype(doc.DocType()) && orgdirectory.IsManagedDirectoryDoc(doc) {
		logger.WithNamespace("data").Warnf("rejecting create on managed organization directory document %s/%s: %s", doc.DocType(), doc.ID(), managedDirectoryWriteReason)
		return managedDirectoryWriteError()
	}
	return nil
}

func rejectManagedDirectoryUpdate(doc, old *couchdb.JSONDoc) error {
	if !orgdirectory.IsManagedDirectoryDoctype(doc.DocType()) {
		return nil
	}
	if orgdirectory.IsManagedDirectoryDoc(doc) || orgdirectory.IsManagedDirectoryDoc(old) {
		logger.WithNamespace("data").Warnf("rejecting update on managed organization directory document %s/%s: %s", doc.DocType(), doc.ID(), managedDirectoryWriteReason)
		return managedDirectoryWriteError()
	}
	return nil
}

func rejectManagedDirectoryDelete(doc *couchdb.JSONDoc) error {
	if orgdirectory.IsManagedDirectoryDoctype(doc.DocType()) && orgdirectory.IsManagedDirectoryDoc(doc) {
		logger.WithNamespace("data").Warnf("rejecting delete on managed organization directory document %s/%s: %s", doc.DocType(), doc.ID(), managedDirectoryWriteReason)
		return managedDirectoryWriteError()
	}
	return nil
}

func allDoctypes(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	if err := middlewares.AllowWholeType(c, permission.GET, consts.Doctypes); err != nil {
		return err
	}

	types, err := couchdb.AllDoctypes(instance)
	if err != nil {
		return err
	}
	var doctypes []string
	for _, typ := range types {
		if permission.CheckReadable(typ) == nil {
			doctypes = append(doctypes, typ)
		}
	}
	return c.JSON(http.StatusOK, doctypes)
}

// GetDoc get a doc by its type and id
func getDoc(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")
	docid := c.Get("docid").(string)

	// Accounts are handled specifically to remove the auth fields
	if doctype == consts.Accounts {
		return getAccount(c)
	}

	if err := permission.CheckReadable(doctype); err != nil {
		return err
	}

	if docid == "" {
		return dbStatus(c)
	}

	if paramIsTrue(c, "revs") {
		return proxy(c, docid)
	}

	var out couchdb.JSONDoc
	err := couchdb.GetDoc(instance, doctype, docid, &out)
	out.Type = doctype
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			if err := middlewares.Allow(c, permission.GET, &out); err != nil {
				return err
			}
		}
		return fixErrorNoDatabaseIsWrongDoctype(err)
	}

	if err := middlewares.Allow(c, permission.GET, &out); err != nil {
		// Allow to read the bitwarden settings document with only a permission
		// bitwarden organizations doctype
		if doctype == consts.Settings && docid == consts.BitwardenSettingsID {
			err = middlewares.AllowWholeType(c, permission.GET, consts.BitwardenOrganizations)
		}
		if err != nil {
			return err
		}
	}

	return c.JSON(http.StatusOK, out.ToMapWithType())
}

// CreateDoc create doc from the json passed as body
func createDoc(c echo.Context) error {
	doctype := c.Param("doctype")
	instance := middlewares.GetInstance(c)

	// Accounts are handled specifically to remove the auth fields
	if doctype == consts.Accounts {
		return createAccount(c)
	}

	doc := couchdb.JSONDoc{Type: doctype}
	if err := json.NewDecoder(c.Request().Body).Decode(&doc.M); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	if err := permission.CheckWritable(doctype); err != nil {
		return err
	}

	if err := rejectManagedDirectoryCreate(&doc); err != nil {
		return err
	}

	if err := middlewares.Allow(c, permission.POST, &doc); err != nil {
		return err
	}

	if err := couchdb.CreateDoc(instance, &doc); err != nil {
		return err
	}

	return c.JSON(http.StatusCreated, echo.Map{
		"ok":   true,
		"id":   doc.ID(),
		"rev":  doc.Rev(),
		"type": doc.DocType(),
		"data": doc.ToMapWithType(),
	})
}

func createNamedDoc(c echo.Context, doc couchdb.JSONDoc) error {
	instance := middlewares.GetInstance(c)

	if err := rejectManagedDirectoryCreate(&doc); err != nil {
		return err
	}

	err := middlewares.Allow(c, permission.POST, &doc)
	if err != nil {
		return err
	}

	err = couchdb.CreateNamedDocWithDB(instance, &doc)
	if err != nil {
		return fixErrorNoDatabaseIsWrongDoctype(err)
	}

	return c.JSON(http.StatusOK, echo.Map{
		"ok":   true,
		"id":   doc.ID(),
		"rev":  doc.Rev(),
		"type": doc.DocType(),
		"data": doc.ToMapWithType(),
	})
}

// UpdateDoc updates the document given in the request or creates a new one with
// the given id.
func UpdateDoc(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")
	docid := c.Get("docid").(string)

	// Accounts are handled specifically to remove the auth fields
	if doctype == consts.Accounts {
		return updateAccount(c)
	}

	var doc couchdb.JSONDoc
	if err := json.NewDecoder(c.Request().Body).Decode(&doc); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	doc.Type = doctype

	if err := permission.CheckWritable(doc.Type); err != nil {
		return err
	}

	if (doc.ID() == "") != (doc.Rev() == "") {
		return jsonapi.Errorf(http.StatusBadRequest,
			"You must either provide an _id and _rev in document (update) or neither (create with fixed id).")
	}

	if doc.ID() != "" && doc.ID() != docid {
		return jsonapi.Errorf(http.StatusBadRequest, "document _id doesnt match url")
	}

	if doc.ID() == "" {
		doc.SetID(docid)
		return createNamedDoc(c, doc)
	}

	if err := rejectManagedDirectoryUpdate(&doc, nil); err != nil {
		return err
	}

	errWhole := middlewares.AllowWholeType(c, permission.PUT, doc.DocType())
	var old *couchdb.JSONDoc
	if orgdirectory.IsManagedDirectoryDoctype(doc.DocType()) || errWhole != nil {
		old = &couchdb.JSONDoc{}
		errFetch := couchdb.GetDoc(instance, doc.DocType(), doc.ID(), old)
		if errFetch != nil {
			return errFetch
		}
		old.Type = doc.DocType()
	}

	if err := rejectManagedDirectoryUpdate(&doc, old); err != nil {
		return err
	}

	if errWhole != nil {
		// check if permissions set allows manipulating old doc
		errOld := middlewares.Allow(c, permission.PUT, old)
		if errOld != nil {
			return errOld
		}

		// also check if permissions set allows manipulating new doc
		errNew := middlewares.Allow(c, permission.PUT, &doc)
		if errNew != nil {
			return errNew
		}
	}

	errUpdate := couchdb.UpdateDoc(instance, &doc)
	if errUpdate != nil {
		return fixErrorNoDatabaseIsWrongDoctype(errUpdate)
	}

	return c.JSON(http.StatusOK, echo.Map{
		"ok":   true,
		"id":   doc.ID(),
		"rev":  doc.Rev(),
		"type": doc.DocType(),
		"data": doc.ToMapWithType(),
	})
}

// DeleteDoc deletes the provided document from its database.
func DeleteDoc(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")
	docid := c.Get("docid").(string)
	revHeader := c.Request().Header.Get("If-Match")
	revQuery := c.QueryParam("rev")
	rev := ""

	if revHeader != "" && revQuery != "" && revQuery != revHeader {
		return jsonapi.Errorf(http.StatusBadRequest,
			"If-Match Header and rev query parameters mismatch")
	} else if revHeader != "" {
		rev = revHeader
	} else if revQuery != "" {
		rev = revQuery
	} else {
		return jsonapi.Errorf(http.StatusBadRequest, "delete without revision")
	}

	if err := permission.CheckWritable(doctype); err != nil {
		return err
	}

	var doc couchdb.JSONDoc
	err := couchdb.GetDoc(instance, doctype, docid, &doc)
	if err != nil {
		return err
	}
	doc.Type = doctype
	doc.SetRev(rev)

	if err := rejectManagedDirectoryDelete(&doc); err != nil {
		return err
	}

	err = middlewares.Allow(c, permission.DELETE, &doc)
	if err != nil {
		return err
	}

	err = couchdb.DeleteDoc(instance, &doc)
	if err != nil {
		return fixErrorNoDatabaseIsWrongDoctype(err)
	}

	return c.JSON(http.StatusOK, echo.Map{
		"ok":      true,
		"id":      doc.ID(),
		"rev":     doc.Rev(),
		"type":    doc.DocType(),
		"deleted": true,
	})
}

// DeleteDatabase deletes the doctype's database.
func DeleteDatabase(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")

	if err := permission.CheckWritable(doctype); err != nil {
		return err
	}
	if err := middlewares.AllowWholeType(c, permission.DELETE, doctype); err != nil {
		return err
	}
	if err := couchdb.DeleteDB(instance, doctype); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, echo.Map{
		"ok":      true,
		"deleted": true,
	})
}

func defineIndex(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")

	var definitionRequest map[string]interface{}
	if err := json.NewDecoder(c.Request().Body).Decode(&definitionRequest); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	if err := permission.CheckReadable(doctype); err != nil {
		return err
	}

	if err := middlewares.AllowWholeType(c, permission.GET, doctype); err != nil {
		return err
	}

	result, err := couchdb.DefineIndexRaw(instance, doctype, &definitionRequest)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, result)
}

func findDocuments(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")

	var findRequest map[string]interface{}
	if err := json.NewDecoder(c.Request().Body).Decode(&findRequest); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	if err := permission.CheckReadable(doctype); err != nil {
		return err
	}

	if err := middlewares.AllowWholeType(c, permission.GET, doctype); err != nil {
		return err
	}

	limit, hasLimit := findRequest["limit"].(float64)
	if !hasLimit || limit > consts.MaxItemsPerPageForMango {
		limit = 100
	}
	findRequest["limit"] = limit

	var results []couchdb.JSONDoc
	resp, err := couchdb.FindDocsRaw(instance, doctype, &findRequest, &results)
	if err != nil {
		return err
	}
	// There might be more docs next when the returned docs reached the limit
	next := len(results) >= int(limit)
	out := echo.Map{
		"docs":     results,
		"limit":    limit,
		"next":     next,
		"bookmark": resp.Bookmark,
	}
	if resp.ExecutionStats != nil {
		out["execution_stats"] = resp.ExecutionStats
	}
	if resp.Warning != "" {
		out["warning"] = resp.Warning
	}
	return c.JSON(http.StatusOK, out)
}

func allDocs(c echo.Context) error {
	doctype := c.Param("doctype")
	if err := permission.CheckReadable(doctype); err != nil {
		return err
	}
	if err := middlewares.AllowWholeType(c, permission.GET, doctype); err != nil {
		return err
	}

	if c.QueryParam("Fields") == "" && c.QueryParam("DesignDocs") == "" {
		// Fast path, just proxy the request/response
		return proxy(c, "_all_docs")
	}

	inst := middlewares.GetInstance(c)
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	skip, _ := strconv.Atoi(c.QueryParam("skip"))
	req := &couchdb.AllDocsRequest{
		Descending: c.QueryParam("descending") == "true",
		Limit:      limit,
		Skip:       skip,
		StartKey:   c.QueryParam("startkey"),
		EndKey:     c.QueryParam("endkey"),
	}
	body, err := couchdb.MakeAllDocsRequest(inst, doctype, req)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": err})
	}
	defer body.Close()

	fields := strings.Split(c.QueryParam("Fields"), ",")
	filter := stream.NewAllDocsFilter(fields)
	if c.QueryParam("DesignDocs") == "false" {
		filter.SkipDesignDocs()
	}
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c.Response().WriteHeader(http.StatusOK)
	if err := filter.Stream(body, c.Response()); err != nil {
		inst.Logger().WithNamespace("couchdb").Warnf("error on all_docs: %s", err)
		return err
	}
	return nil
}

func normalDocs(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")
	if err := permission.CheckReadable(doctype); err != nil {
		return err
	}
	if err := middlewares.AllowWholeType(c, permission.GET, doctype); err != nil {
		return err
	}
	skip, err := strconv.ParseInt(c.QueryParam("skip"), 10, 64)
	if err != nil || skip < 0 {
		skip = 0
	}
	bookmark := c.QueryParam("bookmark")
	limit, err := strconv.ParseInt(c.QueryParam("limit"), 10, 64)
	if err != nil || limit < 0 || limit > consts.MaxItemsPerPageForMango {
		limit = 100
	}
	executionStats, err := strconv.ParseBool(c.QueryParam("execution_stats"))
	if err != nil {
		executionStats = false
	}
	res, err := couchdb.NormalDocs(instance, doctype, int(skip), int(limit), bookmark, executionStats)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, res)
}

func getDesignDoc(c echo.Context) error {
	doctype := c.Param("doctype")
	ddoc := c.Param("designdocid")

	if err := permission.CheckReadable(doctype); err != nil {
		return err
	}
	if err := middlewares.AllowWholeType(c, permission.GET, doctype); err != nil {
		return err
	}
	return proxy(c, "_design/"+ddoc)
}

func getDesignDocs(c echo.Context) error {
	doctype := c.Param("doctype")
	if err := permission.CheckReadable(doctype); err != nil {
		return err
	}
	if err := middlewares.AllowWholeType(c, permission.GET, doctype); err != nil {
		return err
	}
	return proxy(c, "_design_docs")
}

func copyDesignDoc(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")
	ddoc := c.Param("designdocid")

	header := c.Request().Header
	destination := header.Get("Destination")
	if destination == "" {
		return c.JSON(http.StatusBadRequest, "You must set a Destination header")
	}
	if err := permission.CheckReadable(doctype); err != nil {
		return err
	}
	if err := middlewares.AllowWholeType(c, permission.GET, doctype); err != nil {
		return err
	}
	path := "_design/" + ddoc
	res, err := couchdb.Copy(instance, doctype, path, destination)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, res)
}

func deleteDesignDoc(c echo.Context) error {
	doctype := c.Param("doctype")
	ddoc := c.Param("designdocid")

	if err := permission.CheckReadable(doctype); err != nil {
		return err
	}
	if err := middlewares.AllowWholeType(c, permission.GET, doctype); err != nil {
		return err
	}
	if c.QueryParam("rev") == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "You must pass a rev param",
		})
	}
	if !couchdb.CheckDesignDocCanBeDeleted(doctype, ddoc) {
		return c.JSON(http.StatusForbidden, echo.Map{
			"error": "This design doc cannot be deleted",
		})
	}
	return proxy(c, "_design/"+ddoc)
}

// mostly just to prevent couchdb crash on replications
func dataAPIWelcome(c echo.Context) error {
	return c.JSON(http.StatusOK, echo.Map{
		"message": "welcome to a cozy API",
	})
}

func couchdbStyleErrorHandler(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		err := next(c)
		if err == nil {
			return nil
		}

		if ce, ok := err.(*couchdb.Error); ok {
			return c.JSON(ce.StatusCode, ce.JSON())
		}

		if he, ok := err.(*echo.HTTPError); ok {
			return c.JSON(he.Code, echo.Map{"error": he.Error()})
		}

		if je, ok := err.(*jsonapi.Error); ok {
			return c.JSON(je.Status, echo.Map{"error": je.Error()})
		}

		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}
}

// Routes sets the routing for the data service
func Routes(router *echo.Group) {
	router.Use(couchdbStyleErrorHandler)

	// API Routes that don't depend on a doctype
	router.GET("/", dataAPIWelcome)
	router.GET("/_all_doctypes", allDoctypes)

	// API Routes under /:doctype
	group := router.Group("/:doctype", ValidDoctype)

	replicationRoutes(group)
	files.ReferencesRoutes(group)
	files.NotSynchronizedOnRoutes(group)

	group.GET("/:docid", getDoc)
	group.PUT("/:docid", UpdateDoc)
	group.DELETE("/:docid", DeleteDoc)
	group.POST("/", createDoc)
	group.GET("/_all_docs", allDocs)
	group.POST("/_all_docs", allDocs)
	group.GET("/_normal_docs", normalDocs)
	group.POST("/_index", defineIndex)
	group.POST("/_find", findDocuments)

	group.GET("/_design/:designdocid", getDesignDoc)
	group.GET("/_design_docs", getDesignDocs)
	group.POST("/_design/:designdocid/copy", copyDesignDoc)
	group.DELETE("/_design/:designdocid", deleteDesignDoc)

	group.DELETE("/", DeleteDatabase)
}
