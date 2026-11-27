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
)

// Error is an error returned by OpenSearch.
type Error struct {
	Status int
	Type   string
	Reason string
	// DocID is set when the error is about one document of a bulk request.
	DocID string
}

func (e *Error) Error() string {
	if e.DocID != "" {
		return fmt.Sprintf("opensearch: %d %s on %s: %s", e.Status, e.Type, e.DocID, e.Reason)
	}
	return fmt.Sprintf("opensearch: %d %s: %s", e.Status, e.Type, e.Reason)
}

// Options configures the connection to an OpenSearch cluster.
type Options struct {
	URL         *url.URL
	Username    string
	Password    string
	IndexPrefix string
	HTTPClient  *http.Client // Defaults to http.DefaultClient.
}

// Client sends requests to an OpenSearch cluster.
type Client struct {
	opts Options
}

// NewClient returns a client for the given configuration, or nil when search is
// not configured.
func NewClient(opts Options) *Client {
	if opts.URL == nil {
		return nil
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	return &Client{opts: opts}
}

// IndexName returns the name of the index in the cluster, with the prefix.
func (c *Client) IndexName(name string) string {
	if c.opts.IndexPrefix == "" {
		return name
	}
	return c.opts.IndexPrefix + "-" + name
}

// CreateIndex creates an index with the given settings and mappings.
func (c *Client) CreateIndex(ctx context.Context, name string, body any) error {
	return c.doJSON(ctx, http.MethodPut, "/"+c.IndexName(name), body, nil)
}

// Bulk sends NDJSON actions to an index. The action lines must not set
// _index. OpenSearch answers 200 even when some actions fail, so a failed
// action is returned as an error: one that can be retried (429 or 5xx) if
// there is one, else the first one.
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
	var first *Error
	for _, item := range res.Items {
		for _, r := range item {
			if r.Error == nil {
				continue
			}
			err := &Error{Status: r.Status, Type: r.Error.Type, Reason: r.Error.Reason, DocID: r.ID}
			if r.Status == http.StatusTooManyRequests || r.Status >= 500 {
				return err
			}
			if first == nil {
				first = err
			}
		}
	}
	if first != nil {
		return first
	}
	return &Error{Status: http.StatusInternalServerError, Reason: "bulk failed"}
}

type bulkItemResult struct {
	ID     string     `json:"_id"`
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

// SearchWithPreference runs a search like Search, on the shard copies chosen
// by preference: the searches with the same preference get the same scores.
func (c *Client) SearchWithPreference(ctx context.Context, index, preference string, body, out any) error {
	path := "/" + c.IndexName(index) + "/_search?preference=" + url.QueryEscape(preference)
	return c.doJSON(ctx, http.MethodPost, path, body, out)
}

// DeleteByQuery starts an OpenSearch task that deletes the documents of an
// index matching the query, at most requestsPerSecond documents per second,
// and does not wait for it: the failures of the task are not returned.
func (c *Client) DeleteByQuery(ctx context.Context, index string, query any, requestsPerSecond int) error {
	body := map[string]any{"query": query}
	path := fmt.Sprintf("/%s/_delete_by_query?conflicts=proceed&wait_for_completion=false&requests_per_second=%d",
		c.IndexName(index), requestsPerSecond)
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
	p, q, _ := strings.Cut(path, "?")
	endpoint := url.URL{Path: p}
	u := c.opts.URL.JoinPath(endpoint.EscapedPath())
	u.RawQuery = q
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if c.opts.Username != "" {
		req.SetBasicAuth(c.opts.Username, c.opts.Password)
	}
	res, err := c.opts.HTTPClient.Do(req)
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
		e.Type = detail.Type
		e.Reason = detail.Reason
	} else {
		// Some errors (e.g. 404 on an unknown route) are a plain string
		_ = json.Unmarshal(body.Error, &e.Reason)
	}
	return e
}
