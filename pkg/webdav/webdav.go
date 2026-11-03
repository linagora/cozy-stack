// Package webdav is a webdav client library.
package webdav

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	"github.com/labstack/echo/v4"
)

type Client struct {
	Scheme   string
	Host     string
	Username string
	Password string
	BasePath string
	Logger   *logger.Entry
}

// unexpectedStatus logs the surprising response and returns the
// internal-server-error sentinel. The req() helper already records
// every status at INFO; routing the breaker cases through ERROR
// makes them grep-able when a caller surfaces an opaque 500.
func (c *Client) unexpectedStatus(method, path string, status int) error {
	c.Logger.Errorf("%s %s %s: unexpected status %d", method, c.Host, path, status)
	return ErrInternalServerError
}

// unexpectedMoveStatus is a Move/Copy variant that also logs the
// destination path; the source alone is rarely enough to identify
// why the operation failed.
func (c *Client) unexpectedMoveStatus(method, oldPath, newPath string, status int) error {
	c.Logger.Errorf("%s %s %s -> %s: unexpected status %d", method, c.Host, oldPath, newPath, status)
	return ErrInternalServerError
}

func (c *Client) Mkcol(path string) error {
	res, err := c.req("MKCOL", path, 0, nil, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case 201:
		return nil
	case 401, 403:
		return ErrInvalidAuth
	case 405:
		return ErrAlreadyExist
	case 409:
		return ErrParentNotFound
	default:
		return c.unexpectedStatus("MKCOL", path, res.StatusCode)
	}
}

func (c *Client) Delete(path string) error {
	res, err := c.req("DELETE", path, 0, nil, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case 204:
		return nil
	case 401, 403:
		return ErrInvalidAuth
	case 404:
		return ErrNotFound
	default:
		return c.unexpectedStatus("DELETE", path, res.StatusCode)
	}
}

func (c *Client) Move(oldPath, newPath string) error {
	u := url.URL{
		Scheme: c.Scheme,
		Host:   c.Host,
		Path:   c.BasePath + fixSlashes(newPath),
	}
	headers := map[string]string{
		"Destination": u.String(),
		"Overwrite":   "T",
	}
	res, err := c.req("MOVE", oldPath, 0, headers, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case 201, 204:
		return nil
	case 401, 403:
		return ErrInvalidAuth
	case 404, 409:
		return ErrNotFound
	case 412:
		return ErrAlreadyExist
	default:
		return c.unexpectedMoveStatus("MOVE", oldPath, newPath, res.StatusCode)
	}
}

func (c *Client) Copy(oldPath, newPath string) error {
	u := url.URL{
		Scheme: c.Scheme,
		Host:   c.Host,
		Path:   c.BasePath + fixSlashes(newPath),
	}
	headers := map[string]string{
		"Destination": u.String(),
		"Overwrite":   "F",
	}
	res, err := c.req("COPY", oldPath, 0, headers, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case 201, 204:
		return nil
	case 401, 403:
		return ErrInvalidAuth
	case 404, 409:
		return ErrNotFound
	case 412:
		return ErrAlreadyExist
	default:
		return c.unexpectedMoveStatus("COPY", oldPath, newPath, res.StatusCode)
	}
}

func (c *Client) Put(
	path string,
	contentLength int64,
	headers map[string]string,
	body io.Reader,
) error {
	res, err := c.req("PUT", path, contentLength, headers, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case 201:
		return nil
	case 401, 403:
		return ErrInvalidAuth
	case 405:
		return ErrAlreadyExist
	case 404, 409:
		return ErrParentNotFound
	default:
		return c.unexpectedStatus("PUT", path, res.StatusCode)
	}
}

func (c *Client) Get(path string) (*Download, error) {
	res, err := c.req("GET", path, 0, nil, nil)
	if err != nil {
		return nil, err
	}

	if res.StatusCode == 200 {
		return &Download{
			Content:      res.Body,
			ETag:         res.Header.Get("Etag"),
			Length:       res.Header.Get(echo.HeaderContentLength),
			Mime:         res.Header.Get(echo.HeaderContentType),
			LastModified: res.Header.Get(echo.HeaderLastModified),
		}, nil
	}

	defer res.Body.Close()
	switch res.StatusCode {
	case 401, 403:
		return nil, ErrInvalidAuth
	case 404:
		return nil, ErrNotFound
	default:
		return nil, c.unexpectedStatus("GET", path, res.StatusCode)
	}
}

type Download struct {
	Content      io.ReadCloser
	ETag         string
	Length       string
	Mime         string
	LastModified string
}

func (c *Client) List(dir string) ([]Item, error) {
	dir = fixSlashes(dir)
	headers := map[string]string{
		"Content-Type": "application/xml;charset=UTF-8",
		"Accept":       "application/xml",
		"Depth":        "1",
	}
	payload := strings.NewReader(ListFilesPayload)
	res, err := c.req("PROPFIND", dir, 0, headers, payload)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Flush the body, so that the connection can be reused by keep-alive
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()

	switch res.StatusCode {
	case 200, 207:
		// OK continue the work
	case 401, 403:
		return nil, ErrInvalidAuth
	case 404:
		return nil, ErrNotFound
	default:
		return nil, c.unexpectedStatus("PROPFIND", dir, res.StatusCode)
	}

	// https://docs.nextcloud.com/server/20/developer_manual/client_apis/WebDAV/basic.html#requesting-properties
	var multistatus multistatus
	if err := xml.NewDecoder(res.Body).Decode(&multistatus); err != nil {
		return nil, err
	}

	parent := path.Clean(dir)
	var items []Item
	for _, response := range multistatus.Responses {
		parts := strings.Split(strings.TrimPrefix(response.Href, c.BasePath), "/")
		for i, part := range parts {
			if p, err := url.PathUnescape(part); err == nil {
				parts[i] = p
			}
		}
		// We want only the direct children, not the directory itself, and the
		// server may send hrefs with "." or ".." segments or double slashes.
		href := path.Clean("/" + strings.Join(parts, "/"))
		if href == parent {
			continue
		}
		if path.Dir(href) != parent {
			c.Logger.Warnf("PROPFIND %s: skip %q, not a direct child", dir, response.Href)
			continue
		}

		for _, props := range response.Props {
			// Only looks for the HTTP/1.1 200 OK status
			parts := strings.Split(props.Status, " ")
			if len(parts) < 2 || parts[1] != "200" {
				continue
			}
			item := Item{
				ID:           props.FileID,
				Type:         "directory",
				Href:         href,
				Name:         props.Name,
				TrashedName:  props.TrashedName,
				RestorePath:  "/" + props.RestorePath,
				LastModified: props.LastModified,
				ETag:         props.ETag,
			}
			if props.Type.Local == "" {
				item.Type = "file"
				if props.Size != "" {
					if size, err := strconv.ParseUint(props.Size, 10, 64); err == nil {
						item.Size = size
					}
				}
			}
			items = append(items, item)
		}
	}
	return items, nil
}

// Size returns the recursive byte total of the resource at path, as
// reported by the Nextcloud server's cached `oc:size` property. Equivalent
// to `du -sb` on the path, computed in constant time on the server side
// (no traversal). Supported by any Nextcloud/ownCloud host that serves
// the `http://owncloud.org/ns` WebDAV namespace — confirmed against the
// managed hosts we tested (nextcloud05.webo.cloud, use22.thegood.cloud).
func (c *Client) Size(path string) (uint64, error) {
	path = fixSlashes(path)
	headers := map[string]string{
		"Content-Type": "application/xml;charset=UTF-8",
		"Accept":       "application/xml",
		"Depth":        "0",
	}
	payload := strings.NewReader(sizeFilesPayload)
	res, err := c.req("PROPFIND", path, 0, headers, payload)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()

	switch res.StatusCode {
	case 200, 207:
		// continue
	case 401, 403:
		return 0, ErrInvalidAuth
	case 404:
		return 0, ErrNotFound
	default:
		return 0, c.unexpectedStatus("PROPFIND", path, res.StatusCode)
	}

	var multistatus multistatus
	if err := xml.NewDecoder(res.Body).Decode(&multistatus); err != nil {
		return 0, err
	}
	for _, response := range multistatus.Responses {
		for _, propstat := range response.Props {
			parts := strings.Split(propstat.Status, " ")
			if len(parts) < 2 || parts[1] != "200" {
				continue
			}
			if propstat.OcSize == "" {
				continue
			}
			return strconv.ParseUint(propstat.OcSize, 10, 64)
		}
	}
	return 0, ErrNotFound
}

// sizeFilesPayload asks Nextcloud for only the `oc:size` property on the
// target resource. Depth:0 means the server returns the resource itself,
// not its children, so this is one small round trip regardless of how
// many files the directory contains.
const sizeFilesPayload = `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:prop>
        <oc:size />
  </d:prop>
</d:propfind>
`

type Item struct {
	ID           string
	Type         string
	Href         string
	Name         string
	TrashedName  string
	RestorePath  string
	Size         uint64
	ContentType  string
	LastModified string
	ETag         string
}

type multistatus struct {
	XMLName   xml.Name   `xml:"multistatus"`
	Responses []response `xml:"response"`
}

type response struct {
	Href  string  `xml:"DAV: href"`
	Props []props `xml:"DAV: propstat"`
}

type props struct {
	Status       string   `xml:"status"`
	Type         xml.Name `xml:"prop>resourcetype>collection"`
	Name         string   `xml:"prop>displayname"`
	TrashedName  string   `xml:"prop>trashbin-filename"`
	RestorePath  string   `xml:"prop>trashbin-original-location"`
	Size         string   `xml:"prop>getcontentlength"`
	OcSize       string   `xml:"prop>size"`
	ContentType  string   `xml:"prop>getcontenttype"`
	LastModified string   `xml:"prop>getlastmodified"`
	ETag         string   `xml:"prop>getetag"`
	FileID       string   `xml:"prop>fileid"`
}

const ListFilesPayload = `<?xml version="1.0"?>
<d:propfind  xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">
  <d:prop>
        <d:resourcetype />
        <d:displayname />
        <d:getlastmodified />
        <d:getetag />
        <d:getcontentlength />
        <d:getcontenttype />
        <oc:fileid />
        <nc:trashbin-filename />
        <nc:trashbin-original-location />
  </d:prop>
</d:propfind>
`

func (c *Client) req(
	method, path string,
	contentLength int64,
	headers map[string]string,
	body io.Reader,
) (*http.Response, error) {
	path = c.BasePath + fixSlashes(path)
	u := url.URL{
		Scheme: c.Scheme,
		Host:   c.Host,
		User:   url.UserPassword(c.Username, c.Password),
		Path:   path,
	}
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", build.UserAgent())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	start := time.Now()
	res, err := safehttp.ClientWithKeepAlive.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		c.Logger.Warnf("%s %s %s: %s (%s)", method, c.Host, path, err, elapsed)
		return nil, err
	}
	c.Logger.Infof("%s %s %s: %d (%s)", method, c.Host, path, res.StatusCode, elapsed)
	return res, nil
}

func fixSlashes(s string) string {
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
}
