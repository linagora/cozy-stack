package contact

import (
	"sort"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// Group is a struct for a group of contacts.
type Group struct {
	couchdb.JSONDoc
}

// NewGroup returns a new blank group.
func NewGroup() *Group {
	return &Group{
		JSONDoc: couchdb.JSONDoc{
			M: make(map[string]interface{}),
		},
	}
}

// DocType returns the contact document type
func (g *Group) DocType() string { return consts.Groups }

// Name returns the name of the group
func (g *Group) Name() string {
	name, _ := g.Get("name").(string)
	return name
}

// Color returns the color of the group.
func (g *Group) Color() string {
	color, _ := g.Get("color").(string)
	return color
}

// FindGroup returns the group of contacts stored in database from a given ID
func FindGroup(db prefixer.Prefixer, groupID string) (*Group, error) {
	doc := &Group{}
	err := couchdb.GetDoc(db, consts.Groups, groupID, doc)
	return doc, err
}

// GetAllContacts returns the list of contacts inside this group.
func (g *Group) GetAllContacts(db prefixer.Prefixer) ([]*Contact, error) {
	var docs []*Contact
	req := &couchdb.FindRequest{
		UseIndex: "by-groups",
		Selector: mango.Map{
			"relationships": map[string]interface{}{
				"groups": map[string]interface{}{
					"data": map[string]interface{}{
						"$elemMatch": map[string]interface{}{
							"_id":   g.ID(),
							"_type": consts.Groups,
						},
					},
				},
			},
		},
		Limit: 1000,
	}
	err := couchdb.FindDocs(db, consts.Contacts, req, &docs)
	if err != nil {
		return nil, err
	}

	// XXX I didn't find a way to make a mango request with the correct sort
	less := func(i, j int) bool {
		a := docs[i].SortingKey()
		b := docs[j].SortingKey()
		return a < b
	}
	sort.Slice(docs, less)
	return docs, nil
}
