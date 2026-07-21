package orgdirectory

import (
	"errors"
	"fmt"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

const managedDocsPageSize = 1000

func findManagedContactByEmail(db prefixer.Prefixer, email string) (*contact.Contact, error) {
	matches, err := contact.FindAllByEmail(db, email)
	if errors.Is(err, contact.ErrNotFound) {
		return nil, contact.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return singleManagedContact(matches, "email "+email)
}

func findManagedContactByCozyURL(db prefixer.Prefixer, cozyURL string) (*contact.Contact, error) {
	var docs []*contact.Contact
	req := &couchdb.FindRequest{
		Selector: mango.Map{
			"cozy": map[string]interface{}{
				"$elemMatch": map[string]interface{}{
					"url": cozyURL,
				},
			},
		},
		Limit: 2,
	}
	err := couchdb.FindDocsUnoptimized(db, consts.Contacts, req, &docs)
	if couchdb.IsNoDatabaseError(err) || couchdb.IsNotFoundError(err) {
		return nil, contact.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, contact.ErrNotFound
	}
	return singleManagedContact(docs, "cozy URL "+cozyURL)
}

func singleManagedContact(matches []*contact.Contact, label string) (*contact.Contact, error) {
	var managed []*contact.Contact
	for _, doc := range matches {
		if doc.IsExternal() || IsManagedDirectoryDoc(&doc.JSONDoc) {
			managed = append(managed, doc)
		}
	}
	if len(managed) == 0 {
		return nil, contact.ErrNotFound
	}
	if len(managed) > 1 {
		return nil, fmt.Errorf("multiple managed contacts found for %s", label)
	}
	return managed[0], nil
}

func listManagedGroups(inst *instance.Instance, organizationID string) ([]*contact.Group, error) {
	docs, err := listManagedDocs[contact.Group](inst, consts.Groups, organizationID)
	for _, doc := range docs {
		doc.Type = consts.Groups
	}
	return docs, err
}

func listManagedContacts(inst *instance.Instance, organizationID string) ([]*contact.Contact, error) {
	docs, err := listManagedDocs[contact.Contact](inst, consts.Contacts, organizationID)
	for _, doc := range docs {
		doc.Type = consts.Contacts
	}
	return docs, err
}

func listManagedDocs[T any](inst *instance.Instance, doctype, organizationID string) ([]*T, error) {
	var docs []*T
	var bookmark string
	for {
		var page []*T
		req := managedDocsRequest(organizationID, bookmark)
		res, err := couchdb.FindDocsUnoptimizedRaw(inst, doctype, req, &page)
		if couchdb.IsNoDatabaseError(err) || couchdb.IsNotFoundError(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, page...)

		nextBookmark := ""
		if res != nil {
			nextBookmark = res.Bookmark
		}
		if len(page) < managedDocsPageSize || nextBookmark == "" || nextBookmark == bookmark {
			return docs, nil
		}
		bookmark = nextBookmark
	}
}

func managedDocsRequest(organizationID, bookmark string) *couchdb.FindRequest {
	return &couchdb.FindRequest{
		Selector: mango.And(
			mango.Equal(DirectoryMetadataKey+".managed", true),
			mango.Equal(DirectoryMetadataKey+".organizationId", organizationID),
		),
		Limit:    managedDocsPageSize,
		Bookmark: bookmark,
	}
}
