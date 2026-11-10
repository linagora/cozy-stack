package rag

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/messaging"
	jwt "github.com/golang-jwt/jwt/v5"
)

// When the rag.index.topic exchange is configured, the rag-index job does not
// talk to openRAG for each file: it publishes a message telling the RAG
// indexer (github.com/linagora/rag-indexer) to synchronize the file. The
// indexer then reads, on the file route of the stack, what openRAG must hold
// for the file (nothing, when it answers 404), and makes openRAG match it.
// Reading the current state at processing time, instead of carrying it in the
// message, keeps a message replayed late from undoing a newer one.
const (
	// IndexExchange is the exchange the rag-index job publishes to. The
	// indexer owns its topology: the stack does not declare it.
	IndexExchange = "rag.index.topic"
	// IndexRoutingKey is the routing key of the file messages.
	IndexRoutingKey = "rag.index.file"
	// IndexFilesPath is the prefix of the route on which the indexer reads
	// the state (IndexFilesPath + id) and the content (+ "/content") of a file.
	IndexFilesPath = "/ai/index/files/"

	indexTokenAudience = "rag-index"
	// indexTokenTTL outlives the retries of the indexer (about an hour): a
	// message still in its dead-letter queue after that is replaced by the one
	// the next change of the file publishes.
	indexTokenTTL = 7 * 24 * time.Hour
)

// errInvalidIndexToken is a token that does not give access to the file.
var errInvalidIndexToken = errors.New("invalid rag-index token")

// indexerMessage is the body of the sync messages published for the indexer.
type indexerMessage struct {
	Action    string `json:"action"`
	Partition string `json:"partition"`
	FileID    string `json:"file_id"`
	// DocRev is the revision the message was published for: the indexer echoes
	// it on the error status it reports when it gives up on the message.
	DocRev      string `json:"doc_rev"`
	StateURL    string `json:"state_url"`
	FileToken   string `json:"file_token"`
	CallbackURL string `json:"callback_url"`
	RAGBaseURL  string `json:"rag_base_url"`
	RAGAPIKey   string `json:"rag_api_key"`
}

// brokerEnabled tells whether the files of the instances of the context are
// sent through the indexer: the exchange must be configured, and a RabbitMQ
// node must serve the context, the way the RabbitMQ service picks it.
func brokerEnabled(contextName string) bool {
	cfg := config.GetConfig().RabbitMQ
	if !slices.ContainsFunc(cfg.Exchanges, func(e config.RabbitExchange) bool { return e.Name == IndexExchange }) {
		return false
	}
	node, ok := cfg.Nodes[contextName]
	if !ok || !node.Enabled {
		node = cfg.Nodes[config.DefaultInstanceContext]
	}
	return node.Enabled
}

// publish asks the indexer to synchronize the file with openRAG.
func (ctx *indexContext) publish(fileID, rev string) error {
	token, err := MakeIndexToken(ctx.inst, fileID)
	if err != nil {
		return err
	}
	err = ctx.pub.Publish(context.Background(), messaging.PublishRequest{
		ContextName: ctx.inst.ContextName,
		Exchange:    IndexExchange,
		RoutingKey:  IndexRoutingKey,
		MessageID:   ctx.inst.Domain + "/" + fileID,
		Payload: indexerMessage{
			Action:      "sync",
			Partition:   ctx.inst.Domain,
			FileID:      fileID,
			DocRev:      rev,
			StateURL:    ctx.inst.PageURL(IndexFilesPath+fileID, nil),
			FileToken:   token,
			CallbackURL: ctx.inst.PageURL(IndexStatusPath, nil),
			RAGBaseURL:  ctx.server.URL,
			RAGAPIKey:   ctx.server.APIKey,
		},
	})
	return retryable(err)
}

// MakeIndexToken gives the indexer access to the state and content of one
// file. Its audience is unknown to the permission middleware, so it cannot be
// used on any other route.
func MakeIndexToken(inst *instance.Instance, fileID string) (string, error) {
	now := time.Now()
	return crypto.NewJWT(inst.OAuthSecret, jwt.RegisteredClaims{
		Audience:  jwt.ClaimStrings{indexTokenAudience},
		Issuer:    inst.Domain,
		Subject:   fileID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(indexTokenTTL)),
	})
}

// CheckIndexToken returns an error unless the token was made for the file.
func CheckIndexToken(inst *instance.Instance, token, fileID string) error {
	var claims jwt.RegisteredClaims
	err := crypto.ParseJWT(token, func(*jwt.Token) (interface{}, error) {
		return inst.OAuthSecret, nil
	}, &claims)
	if err != nil {
		return errors.Join(errInvalidIndexToken, err)
	}
	if !slices.Contains(claims.Audience, indexTokenAudience) || claims.Issuer != inst.Domain ||
		claims.Subject != fileID || claims.ExpiresAt == nil {
		return errInvalidIndexToken
	}
	return nil
}

// FileState is what openRAG must hold for a file: what the indexer uploads.
type FileState struct {
	// Name is the filename given to openRAG (a note is sent as markdown).
	Name   string `json:"name"`
	MD5Sum string `json:"md5sum"`
	// WorkspaceIDs are the workspaces of the knowledge base folders
	// containing the file, sorted. Never empty.
	WorkspaceIDs []string          `json:"workspace_ids"`
	Metadata     map[string]string `json:"metadata"`
}

// GetFileState applies the scope rule to the file. It returns an error
// matching os.ErrNotExist when the file must not be on openRAG: gone, trashed,
// or outside every knowledge base folder.
func GetFileState(inst *instance.Instance, fileID string) (*FileState, error) {
	doc, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return nil, err
	}
	if doc.Trashed {
		return nil, os.ErrNotExist
	}
	sc, err := loadScopes(inst, ragLogger(inst))
	if err != nil {
		return nil, err
	}
	parentPath, err := sc.dirs.path(inst.VFS(), doc.DirID)
	if err != nil {
		return nil, err
	}
	desired := sc.desiredFor(parentPath)
	if len(desired) == 0 {
		return nil, os.ErrNotExist
	}
	f := fileInfoFromDoc(doc)
	return &FileState{
		Name:         ragFilename(f),
		MD5Sum:       f.MD5,
		WorkspaceIDs: workspaceIDsForDirs(desired),
		Metadata:     uploadMeta(f),
	}, nil
}

// OpenFileContent returns what openRAG indexes for the file: its content, or
// the markdown a note renders to. The scope is not checked again: the indexer
// reads the state first, and a file leaving the scope in between gets its own
// message.
func OpenFileContent(inst *instance.Instance, fileID string) (io.ReadCloser, error) {
	doc, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return nil, err
	}
	if doc.Trashed {
		return nil, os.ErrNotExist
	}
	_, content, err := resolveContent(inst, fileInfoFromDoc(doc))
	return content, err
}
