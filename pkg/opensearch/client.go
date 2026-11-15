// Package opensearch is a small OpenSearch client with only the calls needed
// by the server-side file search.
package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cozy/cozy-stack/pkg/config/config"
)

// Error is an error returned by OpenSearch.
type Error struct {
	Status int
	Type   string
	Reason string
}

func (e *Error) Error() string {
	return fmt.Sprintf("opensearch: %d %s: %s", e.Status, e.Type, e.Reason)
}

// Client sends requests to an OpenSearch cluster.
type Client struct {
	url         *url.URL
	username    string
	password    string
	indexPrefix string
	http        *http.Client
}

// New returns a client for the given configuration, or nil when search is
// not configured.
func New(cfg config.Search) *Client {
	if cfg.URL == nil {
		return nil
	}
	return &Client{
		url:         cfg.URL,
		username:    cfg.Username,
		password:    cfg.Password,
		indexPrefix: cfg.IndexPrefix,
		http:        cfg.Client,
	}
}

// IndexName returns the name of the index in the cluster, with the prefix.
func (c *Client) IndexName(name string) string {
	if c.indexPrefix == "" {
		return name
	}
	return c.indexPrefix + "-" + name
}

// Ping checks that the cluster is reachable and accepts the credentials.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/", "application/json", nil, nil)
}

// CreateIndex creates an index with the given settings and mappings.
func (c *Client) CreateIndex(ctx context.Context, name string, body any) error {
	return c.doJSON(ctx, http.MethodPut, "/"+c.IndexName(name), body, nil)
}

// Bulk sends NDJSON actions to an index. The action lines must not set
// _index. OpenSearch answers 200 even when some actions fail, so the first
// failed action is returned as an error.
func (c *Client) Bulk(ctx context.Context, index string, ndjson []byte) error {
	var res struct {
		Errors bool                        `json:"errors"`
		Items  []map[string]bulkItemResult `json:"items"`
	}
	path := "/" + c.IndexName(index) + "/_bulk"
	if err := c.do(ctx, http.MethodPost, path, "application/x-ndjson", ndjson, &res); err != nil {
		return err
	}
	if !res.Errors {
		return nil
	}
	for _, item := range res.Items {
		for _, r := range item {
			if r.Error != nil {
				return &Error{Status: r.Status, Type: r.Error.Type, Reason: r.Error.Reason}
			}
		}
	}
	return &Error{Status: http.StatusInternalServerError, Reason: "bulk failed"}
}

type bulkItemResult struct {
	Status int        `json:"status"`
	Error  *errorBody `json:"error"`
}

// Mget fetches documents of an index by their ids. out receives the raw
// OpenSearch response ({"docs": [...]}).
func (c *Client) Mget(ctx context.Context, index string, ids []string, out any) error {
	body := map[string]any{"ids": ids}
	return c.doJSON(ctx, http.MethodPost, "/"+c.IndexName(index)+"/_mget", body, out)
}

// Search runs a search on an index. out receives the raw OpenSearch response.
func (c *Client) Search(ctx context.Context, index string, body, out any) error {
	return c.doJSON(ctx, http.MethodPost, "/"+c.IndexName(index)+"/_search", body, out)
}

// DeleteByQuery deletes the documents of an index that match the query.
func (c *Client) DeleteByQuery(ctx context.Context, index string, query any) error {
	body := map[string]any{"query": query}
	path := "/" + c.IndexName(index) + "/_delete_by_query?conflicts=proceed"
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, "application/json", data, out)
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	u := *c.url
	u.Path = strings.TrimSuffix(u.Path, "/")
	p, q, _ := strings.Cut(path, "?")
	u.Path += p
	u.RawQuery = q
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opensearch: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return parseError(res)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

type errorBody struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

func parseError(res *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	e := &Error{Status: res.StatusCode, Reason: strings.TrimSpace(string(data))}
	if json.Unmarshal(data, &body) != nil || len(body.Error) == 0 {
		return e
	}
	var detail errorBody
	if json.Unmarshal(body.Error, &detail) == nil {
		e.Type, e.Reason = detail.Type, detail.Reason
	} else {
		// Some errors (e.g. 404 on an unknown route) are a plain string
		_ = json.Unmarshal(body.Error, &e.Reason)
	}
	return e
}
