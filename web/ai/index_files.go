package ai

import (
	"errors"
	"net/http"
	"os"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// IndexFileState is the route on which the RAG indexer reads what openRAG
// must hold for a file. A 404 tells it to delete the file from openRAG. It is
// authenticated by the token of the message the rag-index job published.
func IndexFileState(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("file-id")
	if err := rag.CheckIndexToken(inst, middlewares.GetRequestToken(c), fileID); err != nil {
		return jsonapi.Unauthorized(err)
	}
	state, err := rag.GetFileState(inst, fileID)
	if err != nil {
		return indexFileError(err)
	}
	return c.JSON(http.StatusOK, state)
}

// IndexFileContent is the route on which the RAG indexer downloads what it
// uploads to openRAG for a file.
func IndexFileContent(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("file-id")
	if err := rag.CheckIndexToken(inst, middlewares.GetRequestToken(c), fileID); err != nil {
		return jsonapi.Unauthorized(err)
	}
	content, err := rag.OpenFileContent(inst, fileID)
	if err != nil {
		return indexFileError(err)
	}
	defer content.Close()
	return c.Stream(http.StatusOK, echo.MIMEOctetStream, content)
}

func indexFileError(err error) error {
	if errors.Is(err, os.ErrNotExist) || couchdb.IsNotFoundError(err) {
		return jsonapi.NotFound(err)
	}
	return err
}
