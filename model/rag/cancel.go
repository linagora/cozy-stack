package rag

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// A query is cancelled through the cache of the stack, shared by the stack
// servers when it is Redis: the job of the query does not run on the server
// that receives the request of the user.
const (
	// cancelTTL is the timeout of the rag-query jobs: a cancel request is
	// useless after it.
	cancelTTL = 15 * time.Minute
	// cancelPollInterval is how often a running query looks for a cancel
	// request.
	cancelPollInterval = 250 * time.Millisecond
)

func cancelKey(inst *instance.Instance, conversationID string) string {
	return "rag-chat-cancel:" + inst.Domain + ":" + conversationID
}

// CancelChat asks the query that answers the last message of the conversation
// to stop: it stops openRAG, and nothing is saved nor published but a `done`
// event. It has no effect when the last message already has its answer, so
// that it never cancels a later message.
func CancelChat(inst *instance.Instance, conversationID string) error {
	var chat ChatConversation
	if err := couchdb.GetDoc(inst, consts.ChatConversations, conversationID, &chat); err != nil {
		return err
	}
	if len(chat.Messages) == 0 {
		return nil
	}
	last := chat.Messages[len(chat.Messages)-1]
	if last.Role != UserRole {
		return nil
	}
	config.GetConfig().CacheStorage.Set(cancelKey(inst, conversationID), []byte(last.ID), cancelTTL)
	return nil
}

// cancelWatcher cancels the context of a query when the user asks for it.
type cancelWatcher struct {
	cancelled atomic.Bool
}

// watchCancel returns a context cancelled when the user cancels the query
// answering the message msgID, and the watcher telling whether it happened.
// The caller must cancel the returned context when the query is over.
func watchCancel(ctx context.Context, inst *instance.Instance, conversationID, msgID string) (context.Context, context.CancelFunc, *cancelWatcher) {
	ctx, cancel := context.WithCancel(ctx)
	w := &cancelWatcher{}
	cache := config.GetConfig().CacheStorage
	key := cancelKey(inst, conversationID)
	requested := func() bool {
		id, ok := cache.Get(key)
		return ok && string(id) == msgID
	}
	stop := func() {
		w.cancelled.Store(true)
		cache.Clear(key)
		cancel()
	}
	// The cancel request may come before the job starts
	if requested() {
		stop()
		return ctx, cancel, w
	}
	go func() {
		ticker := time.NewTicker(cancelPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if requested() {
					stop()
					return
				}
			}
		}
	}()
	return ctx, cancel, w
}

// Cancelled tells whether the user has cancelled the query.
func (w *cancelWatcher) Cancelled() bool {
	return w.cancelled.Load()
}
