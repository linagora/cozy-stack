package permission

import (
	"encoding/json"
	"strings"
)

const verbSep = ","
const allVerbs = "ALL"
const allVerbsLength = 5

// Verb is one of GET,POST,PUT,PATCH,DELETE
type Verb string

// All possible Verbs, a subset of http methods
const (
	GET    = Verb("GET")
	POST   = Verb("POST")
	PUT    = Verb("PUT")
	PATCH  = Verb("PATCH")
	DELETE = Verb("DELETE")
)

var allVerbsOrder = []Verb{GET, POST, PUT, PATCH, DELETE}

// VerbSet is a Set of Verbs
type VerbSet map[Verb]struct{}

// Contains check if VerbSet contains a Verb
func (vs VerbSet) Contains(v Verb) bool {
	if len(vs) == 0 {
		return true // empty set = ALL
	}
	_, has := vs[v]
	return has
}

// ContainsAll check if VerbSet contains all passed verbs
func (vs VerbSet) ContainsAll(verbs VerbSet) bool {
	if len(vs) == 0 {
		return true // empty set = ALL
	}
	if len(verbs) == 0 {
		verbs = ALL
	}

	for v := range verbs {
		_, has := vs[v]
		if !has {
			return false
		}
	}
	return true
}

// ReadOnly returns true if the set contains only the verb GET
func (vs VerbSet) ReadOnly() bool {
	if len(vs) != 1 {
		return false
	}
	_, has := vs[GET]
	return has
}

func (vs VerbSet) String() string {
	out := ""
	if len(vs) == 0 || len(vs) == allVerbsLength {
		return allVerbs
	}
	for _, v := range allVerbsOrder {
		if _, has := vs[v]; has {
			out += verbSep + string(v)
		}
	}
	return out[1:]
}

// MarshalJSON implements json.Marshaller on VerbSet
// the VerbSet is converted to a json array
func (vs VerbSet) MarshalJSON() ([]byte, error) {
	s := make([]string, len(vs))
	i := 0
	for _, v := range allVerbsOrder {
		if _, has := vs[v]; has {
			s[i] = string(v)
			i++
		}
	}
	return json.Marshal(s)
}

// UnmarshalJSON implements json.Unmarshaller on VerbSet
// it expects a json array
func (vs *VerbSet) UnmarshalJSON(b []byte) error {
	*vs = make(VerbSet)
	var s []string
	err := json.Unmarshal(b, &s)
	if err != nil {
		return err
	}
	// empty set means ALL
	for _, v := range s {
		if v == "ALL" {
			return nil
		}
	}
	for _, v := range s {
		switch v {
		case "GET", "POST", "PUT", "PATCH", "DELETE":
			(*vs)[Verb(v)] = struct{}{}
		default:
			return ErrBadScope
		}
	}
	return nil
}

// Merge add verbs to the set
func (vs *VerbSet) Merge(verbs *VerbSet) {
	for v := range *verbs {
		(*vs)[v] = struct{}{}
	}
}

// VerbSplit parse a string into a VerbSet
// Note: this does not check if Verbs are proper HTTP Verbs
// This behaviour is used in @event trigger
func VerbSplit(in string) VerbSet {
	if in == allVerbs {
		return ALL
	}
	verbs := strings.Split(in, verbSep)
	out := make(VerbSet, len(verbs))
	for _, v := range verbs {
		out[Verb(v)] = struct{}{}
	}
	return out
}

// Verbs is a utility function to create VerbSets
func Verbs(verbs ...Verb) VerbSet {
	vs := make(VerbSet, len(verbs))
	for _, v := range verbs {
		vs[v] = struct{}{}
	}
	return vs
}

// ALL : the default VerbSet allows all Verbs
var ALL = Verbs(GET, POST, PUT, PATCH, DELETE)
