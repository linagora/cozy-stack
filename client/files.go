package client

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/client/request"
)

const (
	// DirType is the directory type name
	DirType = "directory"
	// FileType is the file type name
	FileType = "file"
)

// Upload is a struct containing the options of an upload
type Upload struct {
	Name          string
	DirID         string
	FileID        string
	FileRev       string
	ContentMD5    []byte
	Contents      io.Reader
	ContentType   string
	ContentLength int64
	Overwrite     bool
}

// File is the JSON-API file structure
type File struct {
	ID    string `json:"id"`
	Rev   string `json:"rev"`
	Attrs struct {
		Type       string                 `json:"type"`
		Name       string                 `json:"name"`
		DirID      string                 `json:"dir_id"`
		CreatedAt  time.Time              `json:"created_at"`
		UpdatedAt  time.Time              `json:"updated_at"`
		Size       int64                  `json:"size,string"`
		MD5Sum     []byte                 `json:"md5sum"`
		Mime       string                 `json:"mime"`
		Class      string                 `json:"class"`
		Executable bool                   `json:"executable"`
		Encrypted  bool                   `json:"encrypted"`
		Tags       []string               `json:"tags"`
		Metadata   map[string]interface{} `json:"metadata"`
	} `json:"attributes"`
}

// Dir is the JSON-API directory structure
type Dir struct {
	ID    string `json:"id"`
	Rev   string `json:"rev"`
	Attrs struct {
		Type      string                 `json:"type"`
		Name      string                 `json:"name"`
		DirID     string                 `json:"dir_id"`
		Fullpath  string                 `json:"path"`
		CreatedAt time.Time              `json:"created_at"`
		UpdatedAt time.Time              `json:"updated_at"`
		Tags      []string               `json:"tags"`
		Metadata  map[string]interface{} `json:"metadata"`
	} `json:"attributes"`
}

// DirOrFile is the JSON-API file structure used to encapsulate a file or
// directory
type DirOrFile File

// FilePatchAttrs is the attributes in the JSON-API structure for modifying the
// metadata of a file or directory
type FilePatchAttrs struct {
	Name       string    `json:"name,omitempty"`
	DirID      string    `json:"dir_id,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
	Executable bool      `json:"executable,omitempty"`
	Class      string    `json:"class,omitempty"`
}

// FilePatch is the structure used to modify file or directory metadata
type FilePatch struct {
	Rev   string         `json:"-"`
	Attrs FilePatchAttrs `json:"attributes"`
}

// ListChildrenPageSize controls the number of items fetched per request when
// listing directory children. Tests can override this to force pagination.
var ListChildrenPageSize = "100"

// InDrive scopes the client to the given shared drive: file operations go
// through the /sharings/drives/<id> endpoints instead of /files, so the
// stack hosting the drive enforces member authorization. All the operations
// routed through filesPath have a drive-route equivalent (reads, creation,
// metadata patch, upload, overwrite, trash, restore, destroy). It mutates
// and returns the receiver for chaining.
func (c *Client) InDrive(driveID string) *Client {
	c.driveID = driveID
	return c
}

// filesPath prefixes p with the files API root: the shared-drive endpoints
// when the client is scoped to a drive (InDrive), /files otherwise.
func (c *Client) filesPath(p string) string {
	if c.driveID != "" {
		return "/sharings/drives/" + url.PathEscape(c.driveID) + p
	}
	return "/files" + p
}

// GetFileByID returns a File given the specified ID
func (c *Client) GetFileByID(id string) (*File, error) {
	res, err := c.Req(&request.Options{
		Method: "GET",
		Path:   c.filesPath("/" + url.PathEscape(id)),
	})
	if err != nil {
		return nil, err
	}
	return readFile(res)
}

// GetFileByPath returns a File given the specified path
func (c *Client) GetFileByPath(name string) (*File, error) {
	res, err := c.Req(&request.Options{
		Method:  "GET",
		Path:    c.filesPath("/metadata"),
		Queries: url.Values{"Path": {name}},
	})
	if err != nil {
		return nil, err
	}
	return readFile(res)
}

// GetDirByID returns a Dir given the specified ID
func (c *Client) GetDirByID(id string) (*Dir, error) {
	res, err := c.Req(&request.Options{
		Method: "GET",
		Path:   c.filesPath("/" + url.PathEscape(id)),
	})
	if err != nil {
		return nil, err
	}
	return readDir(res)
}

// GetDirByPath returns a Dir given the specified path
func (c *Client) GetDirByPath(name string) (*Dir, error) {
	res, err := c.Req(&request.Options{
		Method:  "GET",
		Path:    c.filesPath("/metadata"),
		Queries: url.Values{"Path": {name}},
	})
	if err != nil {
		return nil, err
	}
	return readDir(res)
}

// GetDirOrFileByPath returns a DirOrFile given the specified path
func (c *Client) GetDirOrFileByPath(name string) (*DirOrFile, error) {
	res, err := c.Req(&request.Options{
		Method:  "GET",
		Path:    c.filesPath("/metadata"),
		Queries: url.Values{"Path": {name}},
	})
	if err != nil {
		return nil, err
	}
	return readDirOrFile(res)
}

// Mkdir creates a directory with the specified path. If the directory's parent
// does not exist, an error is returned.
func (c *Client) Mkdir(name string) (*Dir, error) {
	return c.mkdir(name, "")
}

// Mkdirall creates a directory with the specified path. If the directory's
// parent does not exist, all intermediary parents are created.
func (c *Client) Mkdirall(name string) (*Dir, error) {
	return c.mkdir(name, "true")
}

func (c *Client) mkdir(name string, recur string) (*Dir, error) {
	res, err := c.Req(&request.Options{
		Method: "POST",
		Path:   c.filesPath("/"),
		Queries: url.Values{
			"Path":      {name},
			"Type":      {"directory"},
			"Recursive": {recur},
		},
	})
	if err != nil {
		return nil, err
	}
	return readDir(res)
}

// DownloadByID is used to download a file's content given its ID. It returns
// a io.ReadCloser that you can read from.
func (c *Client) DownloadByID(id string) (io.ReadCloser, error) {
	res, err := c.Req(&request.Options{
		Method: "GET",
		Path:   c.filesPath("/download/" + url.PathEscape(id)),
	})
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// DownloadByPath is used to download a file's content given its path. It
// returns a io.ReadCloser that you can read from.
func (c *Client) DownloadByPath(name string) (io.ReadCloser, error) {
	res, err := c.Req(&request.Options{
		Method:  "GET",
		Path:    c.filesPath("/download"),
		Queries: url.Values{"Path": {name}},
	})
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// Upload is used to upload a new file from an using a Upload instance. If the
// ContentMD5 field is not nil, the file integrity is checked.
func (c *Client) Upload(u *Upload) (*File, error) {
	headers := make(request.Headers)
	if u.ContentMD5 != nil {
		headers["Content-MD5"] = base64.StdEncoding.EncodeToString(u.ContentMD5)
	}
	if u.ContentType != "" {
		headers["Content-Type"] = u.ContentType
	}
	headers["Expect"] = "100-continue"

	opts := &request.Options{
		Body:    u.Contents,
		Headers: headers,
	}
	if u.ContentLength > 0 {
		opts.ContentLength = u.ContentLength
	}

	if u.Overwrite {
		opts.Method = "PUT"
		opts.Path = c.filesPath("/" + url.PathEscape(u.FileID))
		if u.FileRev != "" {
			headers["If-Match"] = u.FileRev
		}
	} else {
		opts.Method = "POST"
		opts.Path = c.filesPath("/" + url.PathEscape(u.DirID))
		opts.Queries = url.Values{
			"Type": {"file"},
			"Name": {u.Name},
		}
	}
	res, err := c.Req(opts)
	if err != nil {
		return nil, err
	}
	return readFile(res)
}

// UpdateAttrsByID is used to update the attributes of a file or directory
// of the specified ID
func (c *Client) UpdateAttrsByID(id string, patch *FilePatch) (*DirOrFile, error) {
	body, err := writeJSONAPI(patch)
	if err != nil {
		return nil, err
	}
	headers := make(request.Headers)
	if patch.Rev != "" {
		headers["If-Match"] = patch.Rev
	}
	res, err := c.Req(&request.Options{
		Method:  "PATCH",
		Path:    c.filesPath("/" + id),
		Body:    body,
		Headers: headers,
	})
	if err != nil {
		return nil, err
	}
	return readDirOrFile(res)
}

// UpdateAttrsByPath is used to update the attributes of a file or directory
// of the specified path
func (c *Client) UpdateAttrsByPath(name string, patch *FilePatch) (*DirOrFile, error) {
	body, err := writeJSONAPI(patch)
	if err != nil {
		return nil, err
	}
	headers := make(request.Headers)
	if patch.Rev != "" {
		headers["If-Match"] = patch.Rev
	}
	res, err := c.Req(&request.Options{
		Method:  "PATCH",
		Path:    c.filesPath("/metadata"),
		Headers: headers,
		Body:    body,
		Queries: url.Values{"Path": {name}},
	})
	if err != nil {
		return nil, err
	}
	return readDirOrFile(res)
}

// Move is used to move a file or directory from a given path to the other
// given path
func (c *Client) Move(from, to string) error {
	doc, err := c.GetDirByPath(path.Dir(to))
	if err != nil {
		return err
	}
	_, err = c.UpdateAttrsByPath(from, &FilePatch{
		Attrs: FilePatchAttrs{
			DirID:     doc.ID,
			Name:      path.Base(to),
			UpdatedAt: time.Now(),
		},
	})
	return err
}

// TrashByID is used to move a file or directory specified by its ID to the
// trash
func (c *Client) TrashByID(id string) error {
	_, err := c.Req(&request.Options{
		Method:     "DELETE",
		Path:       c.filesPath("/" + url.PathEscape(id)),
		NoResponse: true,
	})
	return err
}

// TrashByPath is used to move a file or directory specified by its path to the
// trash
func (c *Client) TrashByPath(name string) error {
	doc, err := c.GetDirOrFileByPath(name)
	if err != nil {
		return err
	}
	return c.TrashByID(doc.ID)
}

// RestoreByID is used to restore a file or directory from the trash given its
// ID
func (c *Client) RestoreByID(id string) error {
	_, err := c.Req(&request.Options{
		Method:     "POST",
		Path:       c.filesPath("/trash/" + url.PathEscape(id)),
		NoResponse: true,
	})
	return err
}

// RestoreByPath is used to restore a file or directory from the trash given its
// path
func (c *Client) RestoreByPath(name string) error {
	doc, err := c.GetDirOrFileByPath(name)
	if err != nil {
		return err
	}
	return c.RestoreByID(doc.ID)
}

// PermanentDeleteByID is used to delete a file or directory specified by its
// ID, not just putting it in the trash
func (c *Client) PermanentDeleteByID(id string) error {
	_, err := c.Req(&request.Options{
		Method:     "PATCH",
		Path:       c.filesPath("/" + url.PathEscape(id)),
		Body:       strings.NewReader(`{"data": {"attributes": {"permanent_delete": true}}}`),
		NoResponse: true,
	})
	return err
}

// PermanentDeleteByPath is used to delete a file or directory specified by its
// path, not just putting it in the trash
func (c *Client) PermanentDeleteByPath(name string) error {
	doc, err := c.GetDirOrFileByPath(name)
	if err != nil {
		return err
	}
	return c.PermanentDeleteByID(doc.ID)
}

// getIncludedPage performs a GET request on reqPath with reqQuery and returns
// the page's included DirOrFile items and the next link (empty if none).
func (c *Client) getIncludedPage(reqPath string, reqQuery url.Values) ([]*DirOrFile, string, error) {
	res, err := c.Req(&request.Options{
		Method:  "GET",
		Path:    reqPath,
		Queries: reqQuery,
	})
	if err != nil {
		return nil, "", err
	}
	var included []*DirOrFile
	var links struct{ Next string }
	if err = readJSONAPILinks(res.Body, &included, &links); err != nil {
		return nil, "", err
	}
	return included, links.Next, nil
}

// ListChildrenByDirID returns all direct child items (files and directories)
// of the directory identified by its ID. It transparently follows pagination
// and returns the complete list.
func (c *Client) ListChildrenByDirID(id string) ([]*DirOrFile, error) {
	reqPath := c.filesPath("/" + url.PathEscape(id))
	reqQuery := url.Values{"page[limit]": {ListChildrenPageSize}}
	var all []*DirOrFile
	for {
		included, next, err := c.getIncludedPage(reqPath, reqQuery)
		if err != nil {
			return nil, err
		}
		all = append(all, included...)
		if next == "" {
			break
		}
		u, err := url.Parse(next)
		if err != nil {
			return nil, err
		}
		reqPath = u.Path
		reqQuery = u.Query()
	}
	return all, nil
}

// WalkFn is the function type used by the walk function.
type WalkFn func(name string, doc *DirOrFile, err error) error

// WalkByPath is used to walk along the filesystem tree originated at the
// specified root path.
func (c *Client) WalkByPath(root string, walkFn WalkFn) error {
	doc, err := c.GetDirOrFileByPath(path.Clean(root))
	root = path.Clean(root)
	if err != nil {
		return walkFn(root, doc, err)
	}
	return walk(c, root, doc, walkFn)
}

func walk(c *Client, name string, doc *DirOrFile, walkFn WalkFn) error {
	isDir := doc.Attrs.Type == DirType

	err := walkFn(name, doc, nil)
	if err != nil {
		if isDir && errors.Is(err, filepath.SkipDir) {
			return nil
		}
		return err
	}

	if !isDir {
		return nil
	}

	reqPath := c.filesPath("/" + url.PathEscape(doc.ID))
	reqQuery := url.Values{"page[limit]": {ListChildrenPageSize}}
	for {
		included, next, err := c.getIncludedPage(reqPath, reqQuery)
		if err != nil {
			return walkFn(name, doc, err)
		}

		for _, d := range included {
			fullpath := path.Join(name, d.Attrs.Name)
			err = walk(c, fullpath, d, walkFn)
			if err != nil && !errors.Is(err, filepath.SkipDir) {
				return err
			}
		}

		if next == "" {
			break
		}
		u, err := url.Parse(next)
		if err != nil {
			return err
		}
		reqPath = u.Path
		reqQuery = u.Query()
	}

	return nil
}

func readDirOrFile(res *http.Response) (*DirOrFile, error) {
	dirOrFile := &DirOrFile{}
	if err := readJSONAPI(res.Body, &dirOrFile); err != nil {
		return nil, err
	}
	return dirOrFile, nil
}

func readFile(res *http.Response) (*File, error) {
	file := &File{}
	if err := readJSONAPI(res.Body, &file); err != nil {
		return nil, err
	}
	if file.Attrs.Type != FileType {
		return nil, fmt.Errorf("not a file")
	}
	return file, nil
}

func readDir(res *http.Response) (*Dir, error) {
	dir := &Dir{}
	if err := readJSONAPI(res.Body, &dir); err != nil {
		return nil, err
	}
	if dir.Attrs.Type != DirType {
		return nil, fmt.Errorf("not a directory")
	}
	return dir, nil
}
