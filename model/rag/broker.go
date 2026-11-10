package rag

import (
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
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
	// RAGBaseURL names the openRAG server of the instance. Its API key is
	// not in the message: the indexer holds the keys of the servers it may
	// call, so that reading the broker does not give access to openRAG.
	RAGBaseURL string `json:"rag_base_url"`
}

// brokerEnabled tells whether the files of the instances of the context are
// sent through the indexer: RabbitMQ must be enabled (else the workers get a
// publisher that always fails), the exchange configured, and a RabbitMQ node
// must serve the context, the way the RabbitMQ service picks it.
func brokerEnabled(contextName string) bool {
	cfg := config.GetConfig().RabbitMQ
	if !cfg.Enabled || !slices.ContainsFunc(cfg.Exchanges, func(e config.RabbitExchange) bool { return e.Name == IndexExchange }) {
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
	err = ctx.pub.Publish(ctx.jobCtx, messaging.PublishRequest{
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
	ID  string `json:"_id"`
	Rev string `json:"_rev"`
	// Name is the filename given to openRAG (a note is sent as markdown).
	Name   string `json:"name"`
	MD5Sum string `json:"md5sum"`
	// WorkspaceIDs are the workspaces of the knowledge base folders
	// containing the file, sorted. Never empty.
	WorkspaceIDs []string          `json:"workspace_ids"`
	Metadata     map[string]string `json:"metadata"`
}

// eligibleFile returns the file, and the knowledge base folders containing
// it, when it must be on openRAG now. It returns an error matching
// os.ErrNotExist when it must not: gone, trashed, of a class whose indexing
// is disabled, or outside every knowledge base folder. The flags and the
// scope are read now, not when the message was published: it may have
// waited in the queue.
func eligibleFile(inst *instance.Instance, fileID string) (*vfs.FileDoc, []string, error) {
	doc, err := inst.VFS().FileByID(fileID)
	if err != nil {
		return nil, nil, err
	}
	if doc.Trashed {
		return nil, nil, os.ErrNotExist
	}
	// An error only means some sources were unreachable, the flags are usable.
	flags, _ := feature.GetFlags(inst)
	if !isClassAllowed(flags, doc.Class) {
		return nil, nil, os.ErrNotExist
	}
	sc, err := loadScopes(inst, ragLogger(inst))
	if err != nil {
		return nil, nil, err
	}
	parentPath, err := sc.dirs.path(inst.VFS(), doc.DirID)
	if err != nil {
		return nil, nil, err
	}
	desired := sc.desiredFor(parentPath)
	if len(desired) == 0 {
		return nil, nil, os.ErrNotExist
	}
	return doc, desired, nil
}

// GetFileState is what openRAG must hold for the file. It returns an error
// matching os.ErrNotExist when the file must not be on openRAG (see
// eligibleFile).
func GetFileState(inst *instance.Instance, fileID string) (*FileState, error) {
	doc, desired, err := eligibleFile(inst, fileID)
	if err != nil {
		return nil, err
	}
	f := fileInfoFromDoc(doc)
	return &FileState{
		ID:           doc.DocID,
		Rev:          doc.Rev(),
		Name:         ragFilename(f),
		MD5Sum:       f.MD5,
		WorkspaceIDs: workspaceIDsForDirs(desired),
		Metadata:     uploadMeta(f),
	}, nil
}

// OpenFileContent returns what openRAG indexes for the file: its content, or
// the markdown a note renders to. It checks the file like GetFileState: a
// token outlives the file's eligibility, and must not give access to a file
// taken out of the knowledge bases.
func OpenFileContent(inst *instance.Instance, fileID string) (io.ReadCloser, error) {
	doc, _, err := eligibleFile(inst, fileID)
	if err != nil {
		return nil, err
	}
	_, content, err := resolveContent(inst, fileInfoFromDoc(doc))
	if err != nil {
		return nil, err
	}
	// The VFS lock cannot be held across the lookup and the opening (both
	// take it, and it is not reentrant), and the afero backend opens a file by
	// its path: a file renamed in between could leave another one at that
	// path. Checking that the file was not moved nor rewritten since the
	// lookup ties the bytes to the authorized file; the indexer retries a 404
	// with the state read again.
	now, err := inst.VFS().FileByID(fileID)
	if err != nil || now.DirID != doc.DirID || now.DocName != doc.DocName || now.InternalID != doc.InternalID || now.Trashed {
		content.Close()
		return nil, os.ErrNotExist
	}
	return content, nil
}
