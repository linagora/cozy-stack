package recents

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
)

// ownerDomainIndex is the name of the index of the rows by the domain of the
// owner of their item. It selects, on the instance of a member, the rows that
// the member holds on the items of one owner.
const ownerDomainIndex = "by-owner-domain"

// ownerDomainPageSize is the number of rows read by request when walking the
// rows of a member on the items of an owner.
const ownerDomainPageSize = 200

// RowsByOwnerDomain returns the rows that the instance holds on the items of
// the given owner, i.e. the rows written for the shared drives of this owner.
// The rows of the items that the instance owns, whose owner_domain is empty,
// are left out. An instance without the database has no row. The rows are read
// by pages so that a member with many rows does not load them all at once.
func RowsByOwnerDomain(inst *instance.Instance, ownerDomain string) ([]*FileDirActivity, error) {
	var rows []*FileDirActivity
	bookmark := ""
	for {
		req := &couchdb.FindRequest{
			UseIndex: ownerDomainIndex,
			Selector: mango.Equal("owner_domain", ownerDomain),
			Limit:    ownerDomainPageSize,
			Bookmark: bookmark,
		}
		var page []*FileDirActivity
		res, err := couchdb.FindDocsRaw(inst, consts.FilesRecents, req, &page)
		if err != nil {
			if couchdb.IsNoDatabaseError(err) {
				return rows, nil
			}
			return nil, err
		}
		rows = append(rows, page...)
		if len(page) < ownerDomainPageSize || res.Bookmark == "" {
			return rows, nil
		}
		bookmark = res.Bookmark
	}
}
