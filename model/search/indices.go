package search

import (
	"context"
	"errors"
	"net/http"

	"github.com/cozy/cozy-stack/pkg/opensearch"
)

// filesIndexBody is the definition of the files_v1 index (ADR 008). The
// n-gram analyzer is also used at search time, so that words longer than the
// grams can match.
var filesIndexBody = map[string]any{
	"settings": map[string]any{
		"number_of_shards":     1,
		"auto_expand_replicas": "0-1",
		"analysis": map[string]any{
			"normalizer": map[string]any{
				"LowercaseAscii": map[string]any{
					"type":   "custom",
					"filter": []string{"lowercase", "asciifolding"},
				},
			},
			"tokenizer": map[string]any{
				"filename_ngram": map[string]any{
					"type":        "ngram",
					"min_gram":    3,
					"max_gram":    4,
					"token_chars": []string{"letter", "digit"},
				},
			},
			"analyzer": map[string]any{
				"filename_index": map[string]any{
					"type":      "custom",
					"tokenizer": "filename_ngram",
					"filter":    []string{"lowercase", "asciifolding"},
				},
			},
		},
	},
	"mappings": map[string]any{
		"dynamic": "strict",
		"properties": map[string]any{
			"resource_key":      keyword,
			"owner_instance_id": keyword,
			"doc_id":            keyword,
			"dir_id":            keyword,
			"scoped_dir_key":    keyword,
			"type":              keyword,
			"name": map[string]any{
				"type":     "text",
				"analyzer": "filename_index",
				"fields": map[string]any{
					"prefix": map[string]any{"type": "search_as_you_type"},
					"exact":  map[string]any{"type": "keyword", "normalizer": "LowercaseAscii"},
				},
			},
			"mime":       keyword,
			"class":      keyword,
			"updated_at": map[string]any{"type": "date"},
			"trashed":    map[string]any{"type": "boolean"},
		},
	},
}

// scopesIndexBody is the definition of the share_scopes_v1 index. The scope
// docs are never searched by their folder keys, so those keys are only kept
// in _source.
var scopesIndexBody = map[string]any{
	"settings": map[string]any{
		"number_of_shards":     1,
		"auto_expand_replicas": "0-all",
	},
	"mappings": map[string]any{
		"dynamic": "strict",
		"properties": map[string]any{
			"sharing_id":               keyword,
			"owner_instance_id":        keyword,
			"root_dir_id":              keyword,
			"active_member_principals": keyword,
			"subtree_dir_keys":         map[string]any{"type": "keyword", "index": false, "doc_values": false},
		},
	},
}

var keyword = map[string]any{"type": "keyword"}

// EnsureIndices creates the search indices that are missing.
func EnsureIndices(ctx context.Context, client *opensearch.Client) error {
	for name, body := range map[string]any{FilesIndex: filesIndexBody, ScopesIndex: scopesIndexBody} {
		err := client.CreateIndex(ctx, name, body)
		var osErr *opensearch.Error
		if errors.As(err, &osErr) && osErr.Status == http.StatusBadRequest &&
			osErr.Type == "resource_already_exists_exception" {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
