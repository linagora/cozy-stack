package permission

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
)

// Set is a Set of rule
type Set []Rule

// MarshalScopeString transforms a Set into a string for Oauth Scope
// (a space separated concatenation of its rules)
func (s Set) MarshalScopeString() (string, error) {
	out := ""
	if len(s) == 0 {
		return "", nil
	}
	for _, r := range s {
		scope, err := r.MarshalScopeString()
		if err != nil {
			return "", err
		}
		out += " " + scope
	}
	return out[1:], nil
}

// UnmarshalScopeString parse a Scope string into a permission Set
func UnmarshalScopeString(in string) (Set, error) {
	if in == "" {
		return nil, ErrBadScope
	}

	parts := strings.Split(in, ruleSep)
	out := make(Set, len(parts))

	for i, p := range parts {
		s, err := UnmarshalRuleString(p)
		if err != nil {
			return nil, err
		}
		out[i] = s
	}

	return out, nil
}

// MarshalJSON implements json.Marshaller on Set. Note that the JSON
// representation is a key-value object, but the golang Set is an ordered
// slice. In theory, JSON objects have no order on their keys, but here, we try
// to keep the same order on decoding/encoding.
// See docs/permissions.md for more details on the structure.
func (s Set) MarshalJSON() ([]byte, error) {
	if len(s) == 0 {
		return []byte("{}"), nil
	}
	buf := make([]byte, 0, 4096)

	for i, r := range s {
		title := r.Title
		if title == "" {
			title = "rule" + strconv.Itoa(i)
		}
		key, err := json.Marshal(title)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		buf = append(buf, ',')
		buf = append(buf, key...)
		buf = append(buf, ':')
		buf = append(buf, val...)
	}

	buf[0] = '{'
	buf = append(buf, '}')
	return buf, nil
}

// UnmarshalJSON parses a json formated permission set, in one pass that keeps
// the order of the rules.
func (s *Set) UnmarshalJSON(j []byte) error {
	dec := json.NewDecoder(bytes.NewReader(j))
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if t == nil {
		*s = make(Set, 0)
		return nil
	}
	if t != json.Delim('{') {
		var m map[string]json.RawMessage
		return json.Unmarshal(j, &m) // the same error as for any non-object
	}
	set := make(Set, 0, 8)
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		title, _ := t.(string)
		var r Rule
		if err := dec.Decode(&r); err != nil {
			return err
		}
		r.Title = title
		// A duplicated title has always given a rule per occurrence, all with
		// the last value: keep it, as the rules are a union of permissions.
		for i := range set {
			if set[i].Title == title {
				set[i] = r
			}
		}
		set = append(set, r)
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	*s = set
	return nil
}

// Some returns true if the predicate return true for any of the rule.
func (s Set) Some(predicate func(Rule) bool) bool {
	for _, r := range s {
		if predicate(r) {
			return true
		}
	}
	return false
}

// RuleInSubset returns true if every document allowed by the rule
// is allowed by the set.
func (s *Set) RuleInSubset(r2 Rule) bool {
	if s.IsMaximal() {
		return true
	}

	for _, r := range *s {
		if !MatchType(r, r2.Type) {
			continue
		}

		if !r.Verbs.ContainsAll(r2.Verbs) {
			continue
		}

		// A rule with no values is for the whole doctype, with or without a
		// selector: only a rule of the same kind can contain it.
		if len(r.Values) == 0 {
			return true
		}
		if len(r2.Values) == 0 {
			continue
		}

		if r.Selector != r2.Selector {
			continue
		}

		if r.ValuesContain(r2.Values...) {
			return true
		}
	}

	return false
}

// IsSubSetOf returns true if every document allowed by the set
// would have been allowed by parent.
func (s *Set) IsSubSetOf(parent Set) bool {
	if s.IsMaximal() {
		return false
	}
	if parent.IsMaximal() {
		return true
	}
	for _, r := range *s {
		if !parent.RuleInSubset(r) {
			return false
		}
	}

	return true
}

// HasSameRules returns true if the two sets have exactly the same rules.
func (s Set) HasSameRules(other Set) bool {
	if len(s) != len(other) {
		return false
	}

	for _, rule := range s {
		match := false
		for _, otherRule := range other {
			if reflect.DeepEqual(rule.Values, otherRule.Values) &&
				rule.Selector == otherRule.Selector &&
				rule.Verbs.ContainsAll(otherRule.Verbs) &&
				otherRule.Verbs.ContainsAll(rule.Verbs) &&
				reflect.DeepEqual(otherRule.Type, rule.Type) {
				match = true
				break
			}
		}

		if !match {
			return false
		}
	}

	return true
}

// Diff returns a the differences between two sets.
// Useful to see what rules had been added between a original manifest
// permissions and now.
//
// We are ignoring removed values/verbs between rule 1 and rule 2.
// - At the moment, it onlys show the added values, verbs and rules
func Diff(set1, set2 Set) Set {
	// If sets are the same, do not compute
	if set1.HasSameRules(set2) {
		return set1
	}

	newSet := Set{}

	// Appending not existing rules
	for _, r2 := range set2 {
		found := false

		for _, r := range set1 {
			if r.Title == r2.Title {
				// Rule exist
				found = true
				break
			}
		}

		if !found {
			newSet = append(newSet, r2)
		}
	}

	// Compare each key
	for _, rule1 := range set1 {
		for _, rule2 := range set2 {
			if rule1.Title == rule2.Title { // Same rule, we are going to compute differences
				newRule := Rule{
					Title:  rule1.Title,
					Type:   rule1.Type,
					Verbs:  map[Verb]struct{}{},
					Values: []string{},
				}

				// Handle verbs. Here we are going to find verbs in set2 that
				// are not present in set1, meaning they were added later by an
				// external human action
				for verb2, content2 := range rule2.Verbs {
					if !rule1.Verbs.Contains(verb2) {
						newRule.Verbs[verb2] = content2
					}
				}

				// Handle values
				for _, value2 := range rule2.Values {
					if ok := rule1.ValuesContain(value2); !ok {
						newRule.Values = append(newRule.Values, value2)
					}
				}

				newSet = append(newSet, newRule)
			}
		}
	}
	return newSet
}

// IsMaximal returns true if the permission is valid for everything. Only the
// flagship app should have it, as it is really powerful.
func (s *Set) IsMaximal() bool {
	return s.Some(func(r Rule) bool {
		return isMaximal(r.Type)
	})
}

// MaximalSet returns the maximal permission, for the flagship app. It gives
// access to every endpoint.
func MaximalSet() Set {
	return Set{
		Rule{Type: allDocTypes},
	}
}
