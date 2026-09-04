package rag

import (
	"encoding/json"
	"testing"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/stretchr/testify/assert"
)

func TestScopeContains(t *testing.T) {
	s := &scope{dirID: "kb", path: "/Docs/KB"}
	assert.True(t, s.contains("/Docs/KB"))
	assert.True(t, s.contains("/Docs/KB/sub"))
	assert.True(t, s.contains("/Docs/KB/sub/deeper"))
	assert.False(t, s.contains("/Docs/KB2"))
	assert.False(t, s.contains("/Docs"))
	assert.False(t, s.contains("/Other"))
	assert.False(t, s.contains("/.cozy_trash/Docs/KB/sub"))

	trashed := &scope{dirID: "kb", path: "/.cozy_trash/KB"}
	assert.False(t, trashed.contains("/.cozy_trash/KB"), "a trashed KB folder claims nothing")
	assert.False(t, trashed.contains("/.cozy_trash/KB/sub"))
}

func TestIsGlobalIndexTrigger(t *testing.T) {
	mk := func(worker string, msg interface{}) *job.TriggerInfos {
		raw, _ := json.Marshal(msg)
		return &job.TriggerInfos{WorkerType: worker, Message: raw}
	}
	assert.True(t, isGlobalIndexTrigger(mk("rag-index", IndexMessage{Doctype: "io.cozy.files"})))
	assert.True(t, isGlobalIndexTrigger(&job.TriggerInfos{WorkerType: "rag-index"}), "no message at all")
	assert.False(t, isGlobalIndexTrigger(mk("rag-index", IndexMessage{Doctype: "io.cozy.files", DirID: "kb"})))
	assert.False(t, isGlobalIndexTrigger(mk("rag-index", IndexMessage{Doctype: "io.cozy.files", DirID: "kb", Action: ActionRemove})))
	assert.False(t, isGlobalIndexTrigger(mk("thumbnail", IndexMessage{Doctype: "io.cozy.files"})))
	assert.False(t, isGlobalIndexTrigger(&job.TriggerInfos{WorkerType: "rag-index", Message: []byte("not json")}))
}
