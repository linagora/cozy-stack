package contact

import (
	"regexp"
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

// FindGroupsByText returns local groups whose names contain text, ignoring case.
func FindGroupsByText(db prefixer.Prefixer, text string, limit int) ([]*Group, error) {
	// shortcut: group-name search scans groups, add a search index if directories grow large.
	var groups []*Group
	err := couchdb.FindDocsUnoptimized(db, consts.Groups, &couchdb.FindRequest{
		Selector: mango.Map{"name": mango.Map{"$regex": "(*UTF8)(?i)" + regexp.QuoteMeta(text)}},
		Limit:    limit,
	}, &groups)
	if couchdb.IsNoDatabaseError(err) {
		return nil, nil
	}
	return groups, err
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
	for {
		var page []*Contact
		res, err := couchdb.FindDocsRaw(db, consts.Contacts, req, &page)
		if err != nil {
			return nil, err
		}
		docs = append(docs, page...)
		if len(page) < req.Limit || res.Bookmark == "" || res.Bookmark == req.Bookmark {
			break
		}
		req.Bookmark = res.Bookmark
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
