package job

import (
	"errors"
	"strings"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// EventTrigger implements Trigger for realtime triggered events
type EventTrigger struct {
	*TriggerInfos
	unscheduled chan struct{}
	mask        []permission.Rule
}

// NewEventTrigger returns a new instance of EventTrigger given the specified
// options.
func NewEventTrigger(infos *TriggerInfos) (*EventTrigger, error) {
	args := strings.Split(infos.Arguments, " ")
	rules := make([]permission.Rule, len(args))
	for i, arg := range args {
		rule, err := permission.UnmarshalRuleString(arg)
		if err != nil {
			return nil, err
		}
		rules[i] = rule
	}
	return &EventTrigger{
		TriggerInfos: infos,
		unscheduled:  make(chan struct{}),
		mask:         rules,
	}, nil
}

// Type implements the Type method of the Trigger interface.
func (t *EventTrigger) Type() string {
	return t.TriggerInfos.Type
}

// Schedule implements the Schedule method of the Trigger interface.
func (t *EventTrigger) Schedule() <-chan *JobRequest {
	ch := make(chan *JobRequest)
	go func() {
		sub := realtime.GetHub().Subscriber(t)
		for _, m := range t.mask {
			sub.Subscribe(m.Type)
		}
		defer func() {
			sub.Close()
			close(ch)
		}()
		for {
			select {
			case e := <-sub.Channel:
				found := false
				for _, m := range t.mask {
					if eventMatchRule(e, &m) {
						found = true
						break
					}
				}
				if found {
					if evt, err := t.Infos().JobRequestWithEvent(e); err == nil {
						ch <- evt
					}
				}
			case <-t.unscheduled:
				return
			}
		}
	}()
	return ch
}

// Unschedule implements the Unschedule method of the Trigger interface.
func (t *EventTrigger) Unschedule() {
	close(t.unscheduled)
}

// Infos implements the Infos method of the Trigger interface.
func (t *EventTrigger) Infos() *TriggerInfos {
	return t.TriggerInfos
}

// CombineRequest implements the CombineRequest method of the Trigger interface.
func (t *EventTrigger) CombineRequest() string {
	return suppressPayload
}

func eventMatchRule(e *realtime.Event, rule *permission.Rule) bool {
	// Previously saved triggers must not receive private events either, nor
	// the events of io.cozy.files.recents, which is only served by its
	// dedicated endpoint.
	if e.Doc.DocType() == consts.PublicLinkEvents || e.Doc.DocType() == consts.FilesRecents {
		return false
	}
	if e.Doc.DocType() != rule.Type {
		return false
	}

	if e.Verb == realtime.EventNotify {
		return false
	}

	if !rule.Verbs.Contains(permission.Verb(e.Verb)) {
		return false
	}

	if len(rule.Values) == 0 {
		return true
	}

	if rule.Selector == "" {
		if rule.ValuesContain(e.Doc.ID()) {
			return true
		}
		if e.Doc.DocType() == consts.Files {
			for _, value := range rule.Values {
				var dir vfs.DirDoc
				if err := couchdb.GetDoc(e, consts.Files, value, &dir); err != nil {
					logger.WithDomain(e.Domain).
						WithNamespace("event-trigger").
						Debugf("Cannot find io.cozy.files %s for trigger rule: %s", value, err)
					continue
				}
				// The trigger value was for a file, not a dir, and it should
				// match only on ID, not on path.
				if dir.Type != consts.DirType {
					continue
				}
				if testPath(&dir, e.Doc) {
					return true
				}
				if e.OldDoc != nil {
					if testPath(&dir, e.OldDoc) {
						return true
					}
				}
			}
		}
		return false
	}

	if len(rule.Values) == 1 && rule.Values[0] == "!=" {
		// Selector for a changed value
		if e.Verb != realtime.EventUpdate {
			return true // We consider that the value has changed on create and delete
		}
		if e.OldDoc == nil {
			return false
		}
		if doc, ok := e.Doc.(permission.Fetcher); ok {
			if old, ok := e.OldDoc.(permission.Fetcher); ok {
				return rule.ValuesChanged(old, doc)
			}
		}
	} else {
		// Selector with normal values
		if v, ok := e.Doc.(permission.Fetcher); ok {
			if rule.ValuesMatch(v) {
				return true
			}
			// Particular case where the new doc is not valid but the old one was.
			if e.OldDoc != nil {
				if vOld, okOld := e.OldDoc.(permission.Fetcher); okOld {
					return rule.ValuesMatch(vOld)
				}
			}
		}
	}

	return false
}

// DumpFilePather is a struct made for calling the Path method of a FileDoc and
// relying on the cached fullpath of this document (not trying to rebuild it)
type DumpFilePather struct{}

// FilePath only returns an error saying to not call this method
func (d DumpFilePather) FilePath(doc *vfs.FileDoc) (string, error) {
	logger.WithNamespace("event-trigger").Warn("FilePath method of DumpFilePather has been called")
	return "", errors.New("DumpFilePather FilePath should not have been called")
}

var dumpFilePather = DumpFilePather{}

func testPath(dir *vfs.DirDoc, doc realtime.Doc) bool {
	if d, ok := doc.(*vfs.DirDoc); ok {
		return strings.HasPrefix(d.Fullpath, dir.Fullpath+"/")
	}
	if f, ok := doc.(*vfs.FileDoc); ok {
		if f.Trashed {
			// XXX When a new file is uploaded, a document may be created in
			// couchdb with trashed: true. We should ignore it.
			if strings.HasPrefix(f.DocRev, "1-") {
				return false
			}
			if f.RestorePath != "" {
				return strings.HasPrefix(f.RestorePath, dir.Fullpath+"/")
			}
		}
		p, err := f.Path(dumpFilePather)
		if err != nil {
			return false
		}
		return strings.HasPrefix(p, dir.Fullpath+"/")
	}
	return false
}

var _ Trigger = &EventTrigger{}
