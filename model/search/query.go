package search

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/opensearch"
)

const (
	// DefaultLimit is the number of hits of a page when no limit is given.
	DefaultLimit = 20
	// MaxLimit is the maximal number of hits of a page.
	MaxLimit = 100
)

// termsChunkSize is the maximal number of folder keys in one terms filter, the
// OpenSearch default of index.max_terms_count (a variable for the tests).
var termsChunkSize = 65536

// rankingVersion is in the cursors, with the name of the index: change it
// with the ranking, so that the cursors of the previous one are refused.
const rankingVersion = 1

// ErrInvalidCursor is returned for a cursor that was not given by Search for
// the same caller and query.
var ErrInvalidCursor = errors.New("search: invalid cursor")

// SearchHit is a file or folder found by a search.
type SearchHit struct {
	Hit
	Type      string
	Name      string
	Mime      string
	Class     string
	UpdatedAt string
}

// Results is a page of hits, with the cursor of the next page (empty on the
// last page), and the shared drives the caller is a member of, by sharing id
// (for VisiblePaths).
type Results struct {
	Hits   []SearchHit
	Cursor string
	Drives map[string]Drive
}

// Search looks for the files and folders whose name matches the query, among
// the files of the caller and those of the shared drives they are a member
// of. Trashed files are excluded. Hits are sorted by relevance.
func Search(ctx context.Context, client *opensearch.Client, caller *instance.Instance, q string, limit int, cursor string) (*Results, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	binding := cursorBinding(caller.Domain, q)
	after, err := decodeCursor(cursor, binding)
	if err != nil {
		return nil, err
	}

	driveByKey, drives, err := memberScopes(ctx, client, caller.Domain)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(driveByKey))
	for key := range driveByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var res struct {
		Hits struct {
			Hits []struct {
				Source FileDocument `json:"_source"`
				Sort   []any        `json:"sort"`
			} `json:"hits"`
		} `json:"hits"`
	}
	// One more hit than the page tells if there is a next page. The same shard
	// copies serve all the pages of a caller, so the scores do not differ
	// between copies.
	query := buildQuery(caller.Domain, q, keys, limit+1, after)
	if err := client.SearchWithPreference(ctx, FilesIndex, caller.Domain, query, &res); err != nil {
		return nil, err
	}

	out := &Results{Hits: make([]SearchHit, 0, len(res.Hits.Hits)), Drives: drives}
	if len(res.Hits.Hits) > limit {
		res.Hits.Hits = res.Hits.Hits[:limit]
		if out.Cursor, err = encodeCursor(res.Hits.Hits[limit-1].Sort, binding); err != nil {
			return nil, err
		}
	}
	for _, h := range res.Hits.Hits {
		doc := h.Source
		hit := SearchHit{
			Hit:  Hit{OwnerInstanceID: doc.OwnerInstanceID, DocID: doc.DocID, DirID: doc.DirID},
			Type: doc.Type, Name: doc.Name, Mime: doc.Mime, Class: doc.Class, UpdatedAt: doc.UpdatedAt,
		}
		if doc.OwnerInstanceID != caller.Domain {
			hit.DriveID = driveByKey[doc.ScopedDirKey]
		}
		out.Hits = append(out.Hits, hit)
	}
	return out, nil
}

// maxMemberDrives is the maximal number of drives of a member, the default
// max result window of OpenSearch.
const maxMemberDrives = 10000

// memberScopes finds the scope docs that list the caller as a member, and
// returns their folder keys, with the drive of each key, and the drives. The
// scope docs are written by the instances of the owners: the recipient's copy
// of a sharing is not read.
//
// shortcut: a member of more than maxMemberDrives drives does not find the
// files of the others; page with search_after when that happens.
func memberScopes(ctx context.Context, client *opensearch.Client, domain string) (map[string]string, map[string]Drive, error) {
	var res struct {
		Hits struct {
			Hits []struct {
				Source ScopeDocument `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	query := map[string]any{
		"size":  maxMemberDrives,
		"query": map[string]any{"term": map[string]any{"active_member_principals": domain}},
	}
	if err := client.Search(ctx, ScopesIndex, query, &res); err != nil {
		return nil, nil, err
	}
	docs := make([]ScopeDocument, len(res.Hits.Hits))
	for i, hit := range res.Hits.Hits {
		docs[i] = hit.Source
	}
	slices.SortFunc(docs, func(a, b ScopeDocument) int { return compareScopes(domain, a, b) })
	driveByKey := make(map[string]string)
	drives := make(map[string]Drive, len(docs))
	for _, doc := range docs {
		drives[doc.SharingID] = Drive{OwnerInstanceID: doc.OwnerInstanceID, RootDirID: doc.RootDirID}
		for _, key := range doc.SubtreeDirKeys {
			if _, ok := driveByKey[key]; !ok {
				driveByKey[key] = doc.SharingID
			}
		}
	}
	return driveByKey, drives, nil
}

// compareScopes orders the drives of the caller (domain), so that a folder in
// nested drives is in the first one, as compareDriveScopes does for the
// recents: the drives where the caller can write, then the outermost one (the
// roots of these drives are on a single chain, so it has the most folders),
// then the smallest sharing id.
func compareScopes(domain string, a, b ScopeDocument) int {
	if aRO, bRO := slices.Contains(a.ReadOnlyPrincipals, domain), slices.Contains(b.ReadOnlyPrincipals, domain); aRO != bRO {
		if aRO {
			return 1
		}
		return -1
	}
	return cmp.Or(
		cmp.Compare(len(b.SubtreeDirKeys), len(a.SubtreeDirKeys)),
		cmp.Compare(a.SharingID, b.SharingID),
	)
}

// buildQuery returns the body of the search request on the files index.
func buildQuery(domain, q string, dirKeys []string, limit int, after []any) map[string]any {
	visible := []any{map[string]any{"term": map[string]any{"owner_instance_id": domain}}}
	for chunk := range slices.Chunk(dirKeys, termsChunkSize) {
		visible = append(visible, map[string]any{"terms": map[string]any{"scoped_dir_key": chunk}})
	}
	body := map[string]any{
		"size": limit,
		"query": map[string]any{"bool": map[string]any{
			"must": []any{map[string]any{"bool": map[string]any{
				"should": []any{
					// Substrings, with the n-grams of the name
					map[string]any{"match": map[string]any{"name": map[string]any{
						"query": q, "minimum_should_match": "75%",
					}}},
					// Words being typed
					map[string]any{"multi_match": map[string]any{
						"query": q, "type": "bool_prefix",
						"fields": []string{"name.prefix", "name.prefix._2gram", "name.prefix._3gram"},
					}},
					// The whole name
					map[string]any{"match": map[string]any{"name.exact": map[string]any{
						"query": q, "boost": 4,
					}}},
				},
				"minimum_should_match": 1,
			}}},
			"filter": []any{map[string]any{"bool": map[string]any{
				"should":               visible,
				"minimum_should_match": 1,
			}}},
			"must_not": []any{map[string]any{"term": map[string]any{"trashed": true}}},
		}},
		"sort": []any{
			map[string]any{"_score": "desc"},
			map[string]any{"resource_key": "asc"},
		},
	}
	if after != nil {
		body["search_after"] = after
	}
	return body
}

// cursorBinding ties a cursor to the caller, the query, the index and the
// ranking, as ADR 008 asks: another search refuses it.
func cursorBinding(domain, q string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s\x00%s", FilesIndex, rankingVersion, domain, q))
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

func encodeCursor(sortValues []any, binding string) (string, error) {
	data, err := json.Marshal(append(sortValues, binding))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeCursor(cursor, binding string) ([]any, error) {
	if cursor == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var after []any
	if err := json.Unmarshal(data, &after); err != nil || len(after) != 3 || after[2] != binding {
		return nil, ErrInvalidCursor
	}
	if _, ok := after[0].(float64); !ok {
		return nil, ErrInvalidCursor
	}
	if _, ok := after[1].(string); !ok {
		return nil, ErrInvalidCursor
	}
	return after[:2], nil
}
