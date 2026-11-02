// Package jsonapi is for using the JSON-API format: parsing, serialization,
// checking the content-type, etc.
package jsonapi

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/labstack/echo/v4"
)

// ContentType is the official mime-type for JSON-API
const ContentType = "application/vnd.api+json"

// Document is JSON-API document, identified by the mediatype
// application/vnd.api+json
// See http://jsonapi.org/format/#document-structure
type Document struct {
	Data     *json.RawMessage `json:"data,omitempty"`
	Errors   ErrorList        `json:"errors,omitempty"`
	Links    *LinksList       `json:"links,omitempty"`
	Meta     *Meta            `json:"meta,omitempty"`
	Included []interface{}    `json:"included,omitempty"`
}

// WriteData can be called to write an answer with a JSON-API document
// containing a single object as data into an io.Writer.
func WriteData(w io.Writer, o Object, links *LinksList) error {
	var included []interface{}

	if inc := o.Included(); inc != nil {
		included = make([]interface{}, len(inc))
		for i, o := range inc {
			data, err := MarshalObject(o)
			if err != nil {
				return err
			}
			included[i] = &data
		}
	}

	data, err := MarshalObject(o)
	if err != nil {
		return err
	}

	doc := Document{
		Data:     &data,
		Links:    links,
		Included: included,
	}
	return json.NewEncoder(w).Encode(doc)
}

// Data can be called to send an answer with a JSON-API document containing a
// single object as data
func Data(c echo.Context, statusCode int, o Object, links *LinksList) error {
	resp := c.Response()
	w := compressedWriter(c.Request(), resp)
	defer func() {
		_ = w.Close()
	}()
	resp.WriteHeader(statusCode)
	return WriteData(w, o, links)
}

// DataList can be called to send an multiple-value answer with a
// JSON-API document contains multiple objects.
func DataList(c echo.Context, statusCode int, objs []Object, links *LinksList) error {
	count := len(objs)
	meta := Meta{Count: &count}
	return DataListWithMeta(c, statusCode, meta, objs, links)
}

// DataListWithMeta can be called to send a list of Objects with meta like a
// count, useful to indicate total number of results with pagination.
func DataListWithMeta(c echo.Context, statusCode int, meta Meta, objs []Object, links *LinksList) error {
	objsMarshaled := make([]json.RawMessage, len(objs))
	for i, o := range objs {
		j, err := MarshalObject(o)
		if err != nil {
			return InternalServerError(err)
		}
		objsMarshaled[i] = j
	}

	data, err := json.Marshal(objsMarshaled)
	if err != nil {
		return InternalServerError(err)
	}

	doc := Document{
		Data:  (*json.RawMessage)(&data),
		Meta:  &meta,
		Links: links,
	}

	resp := c.Response()
	w := compressedWriter(c.Request(), resp)
	defer func() {
		_ = w.Close()
	}()
	resp.WriteHeader(statusCode)
	return json.NewEncoder(w).Encode(doc)
}

func compressedWriter(req *http.Request, resp *echo.Response) io.WriteCloser {
	headers := resp.Header()
	headers.Set(echo.HeaderContentType, ContentType)
	headers.Add(echo.HeaderVary, echo.HeaderAcceptEncoding)
	if !acceptGzipEncoding(req) {
		return &nopCloser{resp}
	}
	headers.Set(echo.HeaderContentEncoding, "gzip")
	return gzip.NewWriter(resp)
}

// nopCloser adds a Close method to a io.Writer (io.NopCloser does that for
// io.Reader).
type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }

func acceptGzipEncoding(req *http.Request) bool {
	return strings.Contains(req.Header.Get(echo.HeaderAcceptEncoding), "gzip")
}

// DataRelations can be called to send a Relations page,
// a list of ResourceIdentifier
func DataRelations(c echo.Context, statusCode int, refs []couchdb.DocReference, meta *Meta, links *LinksList, included []Object) error {
	data, err := json.Marshal(refs)
	if err != nil {
		return InternalServerError(err)
	}

	doc := Document{
		Data:  (*json.RawMessage)(&data),
		Meta:  meta,
		Links: links,
	}

	if included != nil {
		includedMarshaled := make([]interface{}, len(included))
		for i, o := range included {
			j, err := MarshalObject(o)
			if err != nil {
				return InternalServerError(err)
			}
			includedMarshaled[i] = &j
		}
		doc.Included = includedMarshaled
	}

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, ContentType)
	resp.WriteHeader(statusCode)
	return json.NewEncoder(resp).Encode(doc)
}

// DataError can be called to send an error answer with a JSON-API document
// containing a single value error.
func DataError(c echo.Context, err *Error) error {
	doc := Document{
		Errors: ErrorList{err},
	}
	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, ContentType)
	resp.WriteHeader(err.Status)
	return json.NewEncoder(resp).Encode(doc)
}

// DataErrorList can be called to send an error answer with a JSON-API document
// containing multiple errors.
func DataErrorList(c echo.Context, errs ...*Error) error {
	doc := Document{
		Errors: errs,
	}
	if len(errs) == 0 {
		panic("jsonapi.DataErrorList called with empty list.")
	}
	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, ContentType)
	resp.WriteHeader(errs[0].Status)
	return json.NewEncoder(resp).Encode(doc)
}

// Bind is used to unmarshal an input JSONApi document. It binds an
// incoming request to a attribute type.
func Bind(body io.Reader, attrs interface{}) (*ObjectMarshalling, error) {
	decoder := json.NewDecoder(body)
	var doc *Document
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if doc == nil || doc.Data == nil {
		return nil, BadJSON()
	}
	var obj *ObjectMarshalling
	if err := json.Unmarshal(*doc.Data, &obj); err != nil {
		return nil, err
	}
	if obj != nil && obj.Attributes != nil && attrs != nil {
		if err := json.Unmarshal(*obj.Attributes, &attrs); err != nil {
			return nil, err
		}
	}
	return obj, nil
}

// BindCompound is used to unmarshal an compound input JSONApi document.
func BindCompound(body io.Reader) ([]*ObjectMarshalling, error) {
	decoder := json.NewDecoder(body)
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if doc.Data == nil {
		return nil, BadJSON()
	}
	var objs []*ObjectMarshalling
	if err := json.Unmarshal(*doc.Data, &objs); err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if obj == nil {
			return nil, BadJSON()
		}
	}
	return objs, nil
}

// BindRelations extracts a Relationships request ( a list of ResourceIdentifier)
func BindRelations(req *http.Request) ([]couchdb.DocReference, error) {
	var out []couchdb.DocReference
	decoder := json.NewDecoder(req.Body)
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if doc.Data == nil {
		return nil, BadJSON()
	}
	// Attempt Unmarshaling either as ResourceIdentifier or []ResourceIdentifier
	if err := json.Unmarshal(*doc.Data, &out); err != nil {
		var ri couchdb.DocReference
		if err = json.Unmarshal(*doc.Data, &ri); err != nil {
			return nil, err
		}
		out = []couchdb.DocReference{ri}
		return out, nil
	}
	return out, nil
}

// PaginationCursorToParams transforms a Cursor into url.Values
// the url.Values contains only keys page[limit] & page[cursor]
// if the cursor is Done, the values will be empty.
func PaginationCursorToParams(cursor couchdb.Cursor) (url.Values, error) {
	v := url.Values{}

	if !cursor.HasMore() {
		return v, nil
	}

	switch c := cursor.(type) {
	case *couchdb.StartKeyCursor:
		cursorObj := []interface{}{c.NextKey, c.NextDocID}
		cursorBytes, err := json.Marshal(cursorObj)
		if err != nil {
			return nil, err
		}
		v.Set("page[limit]", strconv.Itoa(c.Limit))
		v.Set("page[cursor]", string(cursorBytes))

	case *couchdb.SkipCursor:
		v.Set("page[limit]", strconv.Itoa(c.Limit))
		v.Set("page[skip]", strconv.Itoa(c.Skip))
	}

	return v, nil
}

// ExtractPaginationCursor creates a Cursor from context Query.
func ExtractPaginationCursor(c echo.Context, defaultLimit, maxLimit int) (couchdb.Cursor, error) {
	limit := defaultLimit
	if limitString := c.QueryParam("page[limit]"); limitString != "" {
		reqLimit, err := strconv.ParseInt(limitString, 10, 32)
		if err != nil {
			return nil, NewError(http.StatusBadRequest, "page limit is not a number")
		}
		if reqLimit <= 0 {
			return nil, NewError(http.StatusBadRequest, "page limit must be positive")
		}
		limit = int(reqLimit)
	}
	if maxLimit > 0 && limit > maxLimit {
		limit = maxLimit
	}

	if cursor := c.QueryParam("page[cursor]"); cursor != "" {
		var parts []interface{}
		err := json.Unmarshal([]byte(cursor), &parts)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "bad json cursor %s", cursor)
		}

		if len(parts) != 2 {
			return nil, Errorf(http.StatusBadRequest, "bad cursor length %s", cursor)
		}
		nextKey := parts[0]
		nextDocID, ok := parts[1].(string)
		if !ok {
			return nil, Errorf(http.StatusBadRequest, "bad cursor id %s", cursor)
		}

		return couchdb.NewKeyCursor(limit, nextKey, nextDocID), nil
	}

	if skipString := c.QueryParam("page[skip]"); skipString != "" {
		reqSkip, err := strconv.Atoi(skipString)
		if err != nil {
			return nil, NewError(http.StatusBadRequest, "page skip is not a number")
		}
		return couchdb.NewSkipCursor(limit, reqSkip), nil
	}

	return couchdb.NewKeyCursor(limit, nil, ""), nil
}
