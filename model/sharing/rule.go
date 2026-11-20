package sharing

import (
	"strings"
	"unicode/utf8"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

const (
	// ActionRuleNone is used when an add/update/remove should not be
	// replicated to the other cozys
	ActionRuleNone = "none"
	// ActionRulePush is used when an add/update/remove should be replicated
	// only if it happened on the owner's cozy
	ActionRulePush = "push"
	// ActionRuleSync is used when an add/update/remove should be always replicated
	ActionRuleSync = "sync"
	// ActionRuleRevoke is used when a remove should revoke the sharing
	ActionRuleRevoke = "revoke"
)

// Rule describes how the sharing behave when a document matching the rule is
// added, updated or deleted.
type Rule struct {
	Title    string   `json:"title"`
	DocType  string   `json:"doctype"`
	Mime     string   `json:"mime,omitempty"`
	Selector string   `json:"selector,omitempty"`
	Values   []string `json:"values"`
	Local    bool     `json:"local,omitempty"`
	Add      string   `json:"add"`
	Update   string   `json:"update"`
	Remove   string   `json:"remove"`
}

// FilesByID returns true if the rule is for the files by doctype and the
// selector is an id (not a referenced_by). With such a rule, the identifiers
// must be xored before being sent to another cozy instance.
func (r Rule) FilesByID() bool {
	if r.DocType != consts.Files {
		return false
	}
	return isIDSelector(r.Selector)
}

func isIDSelector(selector string) bool {
	return selector == "" || selector == "id" || selector == "_id"
}

// ValidateRules returns an error if the rules are invalid (the doctype is
// missing for example)
func (s *Sharing) ValidateRules() error {
	if len(s.Rules) == 0 {
		return ErrNoRules
	}
	for i, rule := range s.Rules {
		if rule.Title == "" || len(rule.Values) == 0 {
			return ErrInvalidRule
		}
		if permission.CheckDoctypeName(rule.DocType, false) != nil {
			return ErrInvalidRule
		}
		// These delimiters would change the meaning of the event trigger.
		if !utf8.ValidString(rule.Selector) || strings.ContainsAny(rule.Selector, " ,:") {
			return ErrInvalidRule
		}
		for _, value := range rule.Values {
			if !utf8.ValidString(value) || strings.ContainsAny(value, " ,:") {
				return ErrInvalidRule
			}
		}
		if rule.DocType == consts.Files {
			for _, val := range rule.Values {
				if val == consts.RootDirID ||
					val == consts.TrashDirID ||
					val == consts.SharedWithMeDirID {
					return ErrInvalidRule
				}
			}
			// XXX Currently, we only support one file/folder per rule for the id selector
			if isIDSelector(rule.Selector) {
				if len(rule.Values) > 1 {
					return ErrInvalidRule
				}
			}
			if rule.Selector == couchdb.SelectorReferencedBy {
				// For a referenced_by rule, values should be "doctype/docid"
				for _, val := range rule.Values {
					parts := strings.SplitN(val, "/", 2)
					if len(parts) != 2 {
						return ErrInvalidRule
					}
				}
			}
		} else if permission.CheckWritable(rule.DocType) != nil {
			return ErrInvalidRule
		}
		if rule.Add == "" {
			s.Rules[i].Add = ActionRuleNone
			rule.Add = s.Rules[i].Add
		}
		rule.Add = strings.ToLower(rule.Add)
		if rule.Add != ActionRuleNone &&
			rule.Add != ActionRulePush &&
			rule.Add != ActionRuleSync {
			return ErrInvalidRule
		}
		if rule.Update == "" {
			s.Rules[i].Update = ActionRuleNone
			rule.Update = s.Rules[i].Update
		}
		rule.Update = strings.ToLower(rule.Update)
		if rule.Update != ActionRuleNone &&
			rule.Update != ActionRulePush &&
			rule.Update != ActionRuleSync {
			return ErrInvalidRule
		}
		if rule.Remove == "" {
			s.Rules[i].Remove = ActionRuleNone
			rule.Remove = s.Rules[i].Remove
		}
		rule.Remove = strings.ToLower(rule.Remove)
		if rule.Remove != ActionRuleNone &&
			rule.Remove != ActionRulePush &&
			rule.Remove != ActionRuleSync &&
			rule.Remove != ActionRuleRevoke {
			return ErrInvalidRule
		}
	}
	return nil
}

// Accept returns true if the document matches the rule criteria
func (r Rule) Accept(doctype string, doc map[string]interface{}) bool {
	if r.Local || doctype != r.DocType {
		return false
	}
	var obj interface{} = doc
	if r.Selector == "" || r.Selector == "id" {
		obj = doc["_id"]
	} else if doctype == consts.Files && r.Selector == couchdb.SelectorReferencedBy {
		if o, k := doc[couchdb.SelectorReferencedBy].([]map[string]interface{}); k {
			refs := make([]string, len(o))
			for i, ref := range o {
				refs[i] = ref["type"].(string) + "/" + ref["id"].(string)
			}
			obj = refs
		}
	} else {
		keys := strings.Split(r.Selector, ".")
		for _, key := range keys {
			if o, k := obj.(map[string]interface{}); k {
				obj = o[key]
			} else {
				obj = nil
				break
			}
		}
	}
	if val, ok := obj.(string); ok {
		for _, v := range r.Values {
			if v == val {
				return true
			}
		}
	}
	if val, ok := obj.([]string); ok {
		for _, vv := range val {
			for _, v := range r.Values {
				if v == vv {
					return true
				}
			}
		}
	}
	return false
}

// TriggerArgs returns the string that can be used as an argument to create a
// trigger for this rule. The result can be an empty string if the rule doesn't
// need a trigger (a local or one-shot rule).
func (r Rule) TriggerArgs() string {
	if r.Local {
		return ""
	}
	verbs := make([]string, 1, 3)
	// We always need the CREATED verb to have io.cozy.shared for all the
	// shared documents, as the io.cozy.shared documents are needed to
	// accept the updates and deletes later.
	verbs[0] = "CREATED"
	if r.Update == ActionRuleSync || r.Update == ActionRulePush {
		verbs = append(verbs, "UPDATED")
	}
	if r.Remove == ActionRuleSync || r.Remove == ActionRulePush || r.Remove == ActionRuleRevoke {
		verbs = append(verbs, "DELETED")
	}
	if len(verbs) == 0 {
		return ""
	}
	args := r.DocType + ":" + strings.Join(verbs, ",")
	if len(r.Values) > 0 {
		args += ":" + strings.Join(r.Values, ",")
		if r.Selector != "" && r.Selector != "id" {
			args += ":" + r.Selector
		}
	}
	return args
}

// FirstBitwardenOrganizationRule returns the first not-local rules for the
// com.bitwarden.organizations doctype.
func (s *Sharing) FirstBitwardenOrganizationRule() *Rule {
	for i, rule := range s.Rules {
		if !rule.Local && rule.DocType == consts.BitwardenOrganizations {
			return &s.Rules[i]
		}
	}
	return nil
}

// FirstFilesRule returns the first not-local rules for the files doctype.
func (s *Sharing) FirstFilesRule() *Rule {
	for i, rule := range s.Rules {
		if !rule.Local && rule.DocType == consts.Files {
			return &s.Rules[i]
		}
	}
	return nil
}

func (s *Sharing) findRuleForNewDirectory(dir *vfs.DirDoc) (*Rule, int) {
	for i, rule := range s.Rules {
		if rule.Local || rule.DocType != consts.Files {
			continue
		}
		if rule.Selector != couchdb.SelectorReferencedBy {
			return &s.Rules[i], i
		}
		if len(dir.ReferencedBy) == 0 {
			continue
		}
		allFound := true
		for _, ref := range dir.ReferencedBy {
			if !rule.hasReferencedBy(ref) {
				allFound = false
				break
			}
		}
		if allFound {
			return &s.Rules[i], i
		}
	}
	return nil, 0
}

func (s *Sharing) findRuleForNewFile(file *vfs.FileDoc) (*Rule, int) {
	for i, rule := range s.Rules {
		if rule.Local || rule.DocType != consts.Files {
			continue
		}
		if rule.Selector != couchdb.SelectorReferencedBy {
			return &s.Rules[i], i
		}
		if len(file.ReferencedBy) == 0 {
			continue
		}
		allFound := true
		for _, ref := range file.ReferencedBy {
			if !rule.hasReferencedBy(ref) {
				allFound = false
				break
			}
		}
		if allFound {
			return &s.Rules[i], i
		}
	}
	return nil, 0
}

func (s *Sharing) hasExplicitRuleForFile(file *vfs.FileDoc) bool {
	for _, rule := range s.Rules {
		if rule.Local || rule.DocType != consts.Files {
			continue
		}
		if rule.Selector != "" {
			continue
		}
		for _, id := range rule.Values {
			if id == file.DocID {
				return true
			}
		}
	}
	return false
}

// HasSync returns true if the rule has a sync behaviour
func (r *Rule) HasSync() bool {
	return r.Add == ActionRuleSync || r.Update == ActionRuleSync ||
		r.Remove == ActionRuleSync
}

// HasPush returns true if the rule has a push behaviour
func (r *Rule) HasPush() bool {
	return r.Add == ActionRulePush || r.Update == ActionRulePush ||
		r.Remove == ActionRulePush
}

// hasReferencedBy returns true if the rule matches a file that has this reference
func (r *Rule) hasReferencedBy(ref couchdb.DocReference) bool {
	if r.Selector != couchdb.SelectorReferencedBy {
		return false
	}
	v := ref.Type + "/" + ref.ID
	for _, val := range r.Values {
		if val == v {
			return true
		}
	}
	return false
}
