package registry

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeRegistries map[string][]map[string]string // host -> catalogue, already sorted by name

func (f fakeRegistries) RoundTrip(req *http.Request) (*http.Response, error) {
	apps := f[req.URL.Host]
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

func TestProxyListWalkEndsWithSharedSlug(t *testing.T) {
	proxyClient = &http.Client{Transport: fakeRegistries{
		"reg0": {{"slug": "contacts", "name": "n1"}, {"slug": "notes", "name": "n2"}},
		"reg1": {{"slug": "mail", "name": "n0"}, {"slug": "notes", "name": "n2"}, {"slug": "contacts", "name": "n6"}},
	}}
	registries := []*url.URL{
		{Scheme: "https", Host: "reg0", Path: "/"},
		{Scheme: "https", Host: "reg1", Path: "/"},
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 5, "the listing does not end, next_cursor %q", cursor)
		uri := "/registry?sort=name"
		if cursor != "" {
			uri += "&cursor=" + url.QueryEscape(cursor)
		}
		res, err := ProxyList(&http.Request{Method: http.MethodGet, RequestURI: uri}, registries)
		require.NoError(t, err)
		for _, app := range res.Apps {
			seen[ParseSlug(app["slug"])] = true
		}
		if res.PageInfo.NextCursor == "" {
			break
		}
		require.NotEqual(t, cursor, res.PageInfo.NextCursor, "next_cursor does not advance")
		cursor = res.PageInfo.NextCursor
	}
	require.Len(t, seen, 3)
	require.True(t, seen["mail"])
	require.True(t, seen["contacts"])
	require.True(t, seen["notes"])
}

func TestProxyListWalkEndsWithSharedSlug_PaginationLimit1(t *testing.T) {
	proxyClient = &http.Client{Transport: fakeRegistries{
		"reg0": {{"slug": "contacts", "name": "n1"}, {"slug": "notes", "name": "n2"}},
		"reg1": {{"slug": "mail", "name": "n0"}, {"slug": "notes", "name": "n2"}, {"slug": "contacts", "name": "n6"}},
	}}
	registries := []*url.URL{
		{Scheme: "https", Host: "reg0", Path: "/"},
		{Scheme: "https", Host: "reg1", Path: "/"},
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 10, "the listing does not end with limit=1, next_cursor %q", cursor)
		uri := "/registry?sort=name&limit=1"
		if cursor != "" {
			uri += "&cursor=" + url.QueryEscape(cursor)
		}
		res, err := ProxyList(&http.Request{Method: http.MethodGet, RequestURI: uri}, registries)
		require.NoError(t, err)
		for _, app := range res.Apps {
			slug := ParseSlug(app["slug"])
			require.False(t, seen[slug], "slug %s seen more than once", slug)
			seen[slug] = true
		}
		if res.PageInfo.NextCursor == "" {
			break
		}
		require.NotEqual(t, cursor, res.PageInfo.NextCursor, "next_cursor does not advance")
		cursor = res.PageInfo.NextCursor
	}
	require.Len(t, seen, 3)
}

func TestProxyListThreeRegistriesFuzzerScenario(t *testing.T) {
	// 3 registries sharing slugs, reproducing scenario 30 from issue #5136
	proxyClient = &http.Client{Transport: fakeRegistries{
		"reg0": {{"slug": "app1", "name": "n1"}, {"slug": "app2", "name": "n2"}},
		"reg1": {{"slug": "app2", "name": "n2"}, {"slug": "app3", "name": "n3"}, {"slug": "app1", "name": "n5"}},
		"reg2": {{"slug": "app0", "name": "n0"}, {"slug": "app3", "name": "n3"}, {"slug": "app4", "name": "n4"}},
	}}
	registries := []*url.URL{
		{Scheme: "https", Host: "reg0", Path: "/"},
		{Scheme: "https", Host: "reg1", Path: "/"},
		{Scheme: "https", Host: "reg2", Path: "/"},
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 10, "the listing does not end in 3-registry setup, next_cursor %q", cursor)
		uri := "/registry?sort=name"
		if cursor != "" {
			uri += "&cursor=" + url.QueryEscape(cursor)
		}
		res, err := ProxyList(&http.Request{Method: http.MethodGet, RequestURI: uri}, registries)
		require.NoError(t, err)
		for _, app := range res.Apps {
			seen[ParseSlug(app["slug"])] = true
		}
		if res.PageInfo.NextCursor == "" {
			break
		}
		require.NotEqual(t, cursor, res.PageInfo.NextCursor, "next_cursor does not advance")
		cursor = res.PageInfo.NextCursor
	}
	require.Len(t, seen, 5) // app0, app1, app2, app3, app4
	require.True(t, seen["app0"])
	require.True(t, seen["app1"])
	require.True(t, seen["app2"])
	require.True(t, seen["app3"])
	require.True(t, seen["app4"])
}

func TestProxyListReverseSort(t *testing.T) {
	proxyClient = &http.Client{Transport: fakeRegistries{
		"reg0": {{"slug": "contacts", "name": "n1"}, {"slug": "notes", "name": "n2"}},
		"reg1": {{"slug": "mail", "name": "n0"}, {"slug": "notes", "name": "n2"}, {"slug": "contacts", "name": "n6"}},
	}}
	registries := []*url.URL{
		{Scheme: "https", Host: "reg0", Path: "/"},
		{Scheme: "https", Host: "reg1", Path: "/"},
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 5, "the listing does not end with reverse sort, next_cursor %q", cursor)
		uri := "/registry?sort=-name"
		if cursor != "" {
			uri += "&cursor=" + url.QueryEscape(cursor)
		}
		res, err := ProxyList(&http.Request{Method: http.MethodGet, RequestURI: uri}, registries)
		require.NoError(t, err)
		for _, app := range res.Apps {
			seen[ParseSlug(app["slug"])] = true
		}
		if res.PageInfo.NextCursor == "" {
			break
		}
		require.NotEqual(t, cursor, res.PageInfo.NextCursor, "next_cursor does not advance")
		cursor = res.PageInfo.NextCursor
	}
	require.Len(t, seen, 3)
}

func TestProxyListUnmaskedAfterMasked(t *testing.T) {
	proxyClient = &http.Client{Transport: fakeRegistries{
		"reg0": {{"slug": "masked1", "name": "n1"}, {"slug": "masked2", "name": "n2"}},
		"reg1": {
			{"slug": "masked1", "name": "n1"},
			{"slug": "unique1", "name": "n3"},
			{"slug": "masked2", "name": "n2"},
			{"slug": "unique2", "name": "n4"},
		},
	}}
	registries := []*url.URL{
		{Scheme: "https", Host: "reg0", Path: "/"},
		{Scheme: "https", Host: "reg1", Path: "/"},
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 5, "the listing does not end, next_cursor %q", cursor)
		uri := "/registry?sort=name"
		if cursor != "" {
			uri += "&cursor=" + url.QueryEscape(cursor)
		}
		res, err := ProxyList(&http.Request{Method: http.MethodGet, RequestURI: uri}, registries)
		require.NoError(t, err)
		for _, app := range res.Apps {
			seen[ParseSlug(app["slug"])] = true
		}
		if res.PageInfo.NextCursor == "" {
			break
		}
		require.NotEqual(t, cursor, res.PageInfo.NextCursor, "next_cursor does not advance")
		cursor = res.PageInfo.NextCursor
	}
	require.Len(t, seen, 4)
	require.True(t, seen["masked1"])
	require.True(t, seen["masked2"])
	require.True(t, seen["unique1"])
	require.True(t, seen["unique2"])
}
