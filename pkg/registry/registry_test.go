package registry

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeRegistries map[string][]map[string]string

func (f fakeRegistries) RoundTrip(req *http.Request) (*http.Response, error) {
	appsSrc := f[req.URL.Host]
	apps := make([]map[string]string, len(appsSrc))
	copy(apps, appsSrc)

	sortBy := req.URL.Query().Get("sort")
	if sortBy != "" {
		reverse := strings.HasPrefix(sortBy, "-")
		field := strings.TrimPrefix(sortBy, "-")
		sort.Slice(apps, func(i, j int) bool {
			less := apps[i][field] < apps[j][field]
			if reverse {
				return !less
			}
			return less
		})
	}

	cursor, _ := strconv.Atoi(req.URL.Query().Get("cursor"))
	limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 100
	}
	page, next := []map[string]string{}, ""
	if cursor < len(apps) {
		end := cursor + limit
		if end < len(apps) {
			next = strconv.Itoa(end)
		} else {
			end = len(apps)
		}
		page = apps[cursor:end]
	}
	body, _ := json.Marshal(map[string]interface{}{
		"data": page,
		"meta": map[string]interface{}{"count": len(page), "next_cursor": next},
	})
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func TestProxyListPagination(t *testing.T) {
	cases := []struct {
		name       string
		registries fakeRegistries
		urls       []string
		query      string
		expected   []string
	}{
		{
			name: "shared slug across registries with sort=name",
			registries: fakeRegistries{
				"reg0": {{"slug": "contacts", "name": "n1"}, {"slug": "notes", "name": "n2"}},
				"reg1": {{"slug": "mail", "name": "n0"}, {"slug": "notes", "name": "n2"}, {"slug": "contacts", "name": "n6"}},
			},
			urls:     []string{"https://reg0/", "https://reg1/"},
			query:    "/registry?sort=name",
			expected: []string{"mail", "contacts", "notes"},
		},
		{
			name: "hidden duplicates before unique app with limit=1",
			registries: fakeRegistries{
				"reg0": {{"slug": "app1", "name": "n1"}, {"slug": "app2", "name": "n2"}},
				"reg1": {
					{"slug": "app1", "name": "n1"},
					{"slug": "app2", "name": "n2"},
					{"slug": "app3", "name": "n3"},
				},
			},
			urls:     []string{"https://reg0/", "https://reg1/"},
			query:    "/registry?sort=name&limit=1",
			expected: []string{"app1", "app2", "app3"},
		},
		{
			name: "three registries sharing slugs (fuzzer scenario 30)",
			registries: fakeRegistries{
				"reg0": {{"slug": "app1", "name": "n1"}, {"slug": "app2", "name": "n2"}},
				"reg1": {{"slug": "app2", "name": "n2"}, {"slug": "app3", "name": "n3"}, {"slug": "app1", "name": "n5"}},
				"reg2": {{"slug": "app0", "name": "n0"}, {"slug": "app3", "name": "n3"}, {"slug": "app4", "name": "n4"}},
			},
			urls:     []string{"https://reg0/", "https://reg1/", "https://reg2/"},
			query:    "/registry?sort=name",
			expected: []string{"app0", "app1", "app2", "app3", "app4"},
		},
		{
			name: "reverse sort by name",
			registries: fakeRegistries{
				"reg0": {{"slug": "contacts", "name": "n1"}, {"slug": "notes", "name": "n2"}},
				"reg1": {{"slug": "mail", "name": "n0"}, {"slug": "notes", "name": "n2"}, {"slug": "contacts", "name": "n6"}},
			},
			urls:     []string{"https://reg0/", "https://reg1/"},
			query:    "/registry?sort=-name",
			expected: []string{"notes", "contacts", "mail"},
		},
		{
			name: "unmasked entries after masked duplicates with limit=2",
			registries: fakeRegistries{
				"reg0": {{"slug": "dup1", "name": "a1"}, {"slug": "dup2", "name": "a2"}},
				"reg1": {
					{"slug": "dup1", "name": "a1"},
					{"slug": "uniq1", "name": "b1"},
					{"slug": "dup2", "name": "a2"},
					{"slug": "uniq2", "name": "b2"},
				},
			},
			urls:     []string{"https://reg0/", "https://reg1/"},
			query:    "/registry?sort=name&limit=2",
			expected: []string{"dup1", "dup2", "uniq1", "uniq2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyClient = &http.Client{Transport: tc.registries}
			var registries []*url.URL
			for _, u := range tc.urls {
				parsed, err := url.Parse(u)
				require.NoError(t, err)
				registries = append(registries, parsed)
			}

			var got []string
			cursor := ""
			for page := 0; page < 20; page++ {
				uri := tc.query
				if cursor != "" {
					if strings.Contains(uri, "?") {
						uri += "&cursor=" + url.QueryEscape(cursor)
					} else {
						uri += "?cursor=" + url.QueryEscape(cursor)
					}
				}
				res, err := ProxyList(&http.Request{Method: http.MethodGet, RequestURI: uri}, registries)
				require.NoError(t, err)
				for _, app := range res.Apps {
					got = append(got, ParseSlug(app["slug"]))
				}
				if res.PageInfo.NextCursor == "" {
					break
				}
				require.NotEqual(t, cursor, res.PageInfo.NextCursor, "next_cursor should advance")
				cursor = res.PageInfo.NextCursor
			}

			require.Equal(t, tc.expected, got)
		})
	}
}
