package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/httpcache"
	"github.com/labstack/echo/v4"
)

const defaultLimit = 100

// A Version describes a specific release of an application.
type Version struct {
	Slug      string          `json:"slug"`
	Version   string          `json:"version"`
	URL       string          `json:"url"`
	Sha256    string          `json:"sha256"`
	CreatedAt time.Time       `json:"created_at"`
	Size      string          `json:"size"`
	Manifest  json.RawMessage `json:"manifest"`
	TarPrefix string          `json:"tar_prefix"`
}

// A MaintenanceOptions defines options about a maintenance
type MaintenanceOptions struct {
	FlagInfraMaintenance   bool `json:"flag_infra_maintenance"`
	FlagShortMaintenance   bool `json:"flag_short_maintenance"`
	FlagDisallowManualExec bool `json:"flag_disallow_manual_exec"`
}

// An Application describe an application on the registry
type Application struct {
	Slug                 string             `json:"slug"`
	Type                 string             `json:"type"`
	MaintenanceActivated bool               `json:"maintenance_activated,omitempty"`
	MaintenanceOptions   MaintenanceOptions `json:"maintenance_options"`
}

var errVersionNotFound = errors.New("registry: version not found")
var errApplicationNotFound = errors.New("registry: application not found")

var (
	proxyClient = &http.Client{
		Timeout:   10 * time.Second,
		Transport: httpcache.NewMemoryCacheTransport(32),
	}

	maintenanceClient = &http.Client{
		Timeout:   10 * time.Second,
		Transport: httpcache.NewMemoryCacheTransport(32),
	}

	appClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: httpcache.NewMemoryCacheTransport(256),
	}

	latestVersionClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: httpcache.NewMemoryCacheTransport(256),
	}
)

// CacheControl defines whether or not to use caching for the request made to
// the registries.
type CacheControl int

const (
	// WithCache specify caching
	WithCache CacheControl = iota
	// NoCache disables any caching
	NoCache
)

// GetVersion returns a specific version from a slug name
func GetVersion(slug, version string, registries []*url.URL) (*Version, error) {
	requestURI := fmt.Sprintf("/registry/%s/%s",
		url.PathEscape(slug),
		url.PathEscape(version))
	resp, ok, err := fetchUntilFound(latestVersionClient, registries, requestURI, WithCache)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errVersionNotFound
	}
	defer resp.Body.Close()
	var v *Version
	if err = json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// GetLatestVersion returns the latest version available from the list of
// registries by resolving them in sequence using the specified application
// slug and channel name.
func GetLatestVersion(slug, channel string, registries []*url.URL) (*Version, error) {
	requestURI := fmt.Sprintf("/registry/%s/%s/latest",
		url.PathEscape(slug),
		url.PathEscape(channel))
	resp, ok, err := fetchUntilFound(latestVersionClient, registries, requestURI, WithCache)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errVersionNotFound
	}
	defer resp.Body.Close()
	var v *Version
	if err = json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// GetApplication returns an application from his slug
func GetApplication(slug string, registries []*url.URL) (*Application, error) {
	requestURI := fmt.Sprintf("/registry/%s/", slug)
	resp, ok, err := fetchUntilFound(appClient, registries, requestURI, WithCache)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errApplicationNotFound
	}
	defer resp.Body.Close()
	var app *Application
	if err = json.NewDecoder(resp.Body).Decode(&app); err != nil {
		return nil, err
	}
	return app, nil
}

// Proxy will proxy the given request to the registries in sequence and return
// the response as io.ReadCloser when finding a registry returning a HTTP 200OK
// response.
func Proxy(req *http.Request, registries []*url.URL, cache CacheControl) (*http.Response, error) {
	resp, ok, err := fetchUntilFound(proxyClient, registries, req.RequestURI, cache)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, echo.NewHTTPError(http.StatusNotFound)
	}
	return resp, nil
}

// ListMaintenance will proxy the given request to the registries to fetch all
// the apps in maintenance. It takes care to ignore maintenance for apps
// present in another registry space with higher priority.
func ListMaintenance(registries []*url.URL) ([]couchdb.JSONDoc, error) {
	maskedSlugs := make(map[string]struct{})
	apps := make([]couchdb.JSONDoc, 0)
	for i, r := range registries {
		if i != 0 {
			prev := registries[i-1]
			ref := &url.URL{Path: "/registry/slugs"}
			resp, ok, err := fetch(maintenanceClient, prev, ref, WithCache)
			if err != nil {
				return nil, err
			}
			if ok {
				var slugs []string
				if err = json.NewDecoder(resp.Body).Decode(&slugs); err != nil {
					return nil, err
				}
				for _, slug := range slugs {
					maskedSlugs[slug] = struct{}{}
				}
			}
		}

		ref := &url.URL{Path: "/registry/maintenance"}
		resp, ok, err := fetch(maintenanceClient, r, ref, WithCache)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var docs []couchdb.JSONDoc
		if err = json.NewDecoder(resp.Body).Decode(&docs); err != nil {
			return nil, err
		}
		for _, doc := range docs {
			slug, _ := doc.M["slug"].(string)
			if _, masked := maskedSlugs[slug]; !masked {
				apps = append(apps, doc)
			}
		}
	}
	return apps, nil
}

// ProxyList will proxy the given request to the registries by aggregating the
// results along the way. It should be used for list endpoints.
func ProxyList(req *http.Request, registries []*url.URL) (*AppsPaginated, error) {
	ref, err := url.Parse(req.RequestURI)
	if err != nil {
		return nil, err
	}

	var sortBy string
	var sortReverse bool
	var limit int

	cursors := make([]int, len(registries))

	q := ref.Query()
	if v, ok := q["cursor"]; ok {
		splits := strings.Split(v[0], "|")
		for i, s := range splits {
			if i >= len(registries) {
				break
			}
			cursors[i], _ = strconv.Atoi(s)
		}
	}
	if v, ok := q["sort"]; ok {
		sortBy = v[0]
	}
	if len(sortBy) > 0 && sortBy[0] == '-' {
		sortReverse = true
		sortBy = sortBy[1:]
	}
	if sortBy == "" {
		sortBy = "slug"
	}
	if v, ok := q["limit"]; ok {
		limit, _ = strconv.Atoi(v[0])
	}
	if limit <= 0 {
		limit = defaultLimit
	}

	list := newAppsList(ref, registries, cursors, limit)
	if err := list.FetchAll(); err != nil {
		return nil, err
	}
	return list.Paginated(sortBy, sortReverse, limit), nil
}

type appsList struct {
	ref        *url.URL
	list       []map[string]json.RawMessage
	registries []*registryFetchState
	slugs      map[string][]int
	limit      int
}

// PageInfo is the metadata for pagination.
type PageInfo struct {
	Count      int    `json:"count"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// AppsPaginated is a struct for listing apps manifest from the registry, with
// pagination.
type AppsPaginated struct {
	Apps     []map[string]json.RawMessage `json:"data"`
	PageInfo PageInfo                     `json:"meta"`
}

type registryFetchState struct {
	url           *url.URL
	index         int // index in the registries array
	cursor        int // cursor used to fetch the registry
	ended         int // cursor of the last element in the regitry (-1 if unknown)
	maskedOffsets map[int]bool
}

func newAppsList(ref *url.URL, registries []*url.URL, cursors []int, limit int) *appsList {
	if len(registries) != len(cursors) {
		panic("should have same length")
	}
	regStates := make([]*registryFetchState, len(registries))
	for i := range regStates {
		regStates[i] = &registryFetchState{
			index:         i,
			url:           registries[i],
			cursor:        cursors[i],
			ended:         -1,
			maskedOffsets: make(map[int]bool),
		}
	}
	return &appsList{
		ref:        ref,
		limit:      limit,
		list:       make([]map[string]json.RawMessage, 0),
		slugs:      make(map[string][]int),
		registries: regStates,
	}
}

func (a *appsList) FetchAll() error {
	l := len(a.registries)
	for i, r := range a.registries {
		// We fetch the entire registry except for the last one. In practice, the
		// "high-priority" registries should be small and the last one contain the
		// vast majority of the applications.
		fetchAll := i < l-1
		if err := a.fetch(r, fetchAll); err != nil {
			return err
		}
	}
	return nil
}

func (a *appsList) fetch(r *registryFetchState, fetchAll bool) error {
	slugs := a.slugs
	minCursor := r.cursor

	var cursor, limit int
	if fetchAll {
		cursor = 0
		limit = defaultLimit
	} else {
		cursor = r.cursor
		limit = a.limit
	}

	// A negative dimension of the cursor means we already reached the end of the
	// list. There is no need to fetch anymore in that case.
	if !fetchAll && r.cursor < 0 {
		return nil
	}

	added := 0
	for {
		ref := addQueries(removeQueries(a.ref, "cursor", "limit"),
			"cursor", strconv.Itoa(cursor),
			"limit", strconv.Itoa(limit),
		)
		resp, ok, err := fetch(proxyClient, r.url, ref, NoCache)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		defer resp.Body.Close()
		var page AppsPaginated
		if err = json.NewDecoder(resp.Body).Decode(&page); err != nil {
			return err
		}

		for i, obj := range page.Apps {
			objCursor := cursor + i
			slug := ParseSlug(obj["slug"])

			offsets, ok := slugs[slug]
			if !ok {
				offsets = make([]int, len(a.registries))
				slugs[slug] = offsets
			} else {
				r.maskedOffsets[objCursor] = true
			}

			if r.cursor >= 0 && objCursor >= minCursor {
				offsets[r.index] = objCursor + 1
				if !ok && (fetchAll || added < a.limit) {
					a.list = append(a.list, obj)
					added++
				}
			}
		}

		nextCursor := page.PageInfo.NextCursor
		if nextCursor == "" {
			r.ended = cursor + len(page.Apps)
			break
		}

		cursor, _ = strconv.Atoi(nextCursor)
		if !fetchAll && added >= a.limit {
			break
		}
	}

	return nil
}

func ParseSlug(raw json.RawMessage) string {
	var slug string
	if err := json.Unmarshal(raw, &slug); err != nil {
		return ""
	}
	return slug
}

func (a *appsList) Paginated(sortBy string, reverse bool, limit int) *AppsPaginated {
	sort.Slice(a.list, func(i, j int) bool {
		valA := a.list[i][sortBy]
		valB := a.list[j][sortBy]
		cmp := bytes.Compare([]byte(valA), []byte(valB))
		equal := cmp == 0
		less := cmp < 0
		if equal {
			slugA := ParseSlug(a.list[i]["slug"])
			slugB := ParseSlug(a.list[j]["slug"])
			less = slugA < slugB
		}
		if reverse {
			return !less
		}
		return less
	})

	if limit > len(a.list) {
		limit = len(a.list)
	}

	list := a.list[:limit]

	// Copy the original cursor
	cursors := make([]int, len(a.registries))
	for i, reg := range a.registries {
		cursors[i] = reg.cursor
	}

	// Calculation of the next multi-cursor by iterating through the sorted and
	// truncated list and incrementing the dimension of the multi-cursor
	// associated with the objects registry.
	//
	// In the end, we also check if the end value of each dimensions of the
	// cursor reached the end of the list. If so, the dimension is set to -1.
	l := len(a.registries)
	for _, o := range list {
		slug := ParseSlug(o["slug"])
		offsets := a.slugs[slug]

		i := 0
		// This first loop checks the first element >= 0 in the offsets associated
		// to the object. This first non null element is set as the cursor of the
		// dimension.
		for ; i < l; i++ {
			if c := offsets[i]; c > 0 {
				cursors[i] = c
				break
			}
		}
		// We continue the iteration to the next lower-priority dimensions and for
		// non-null ones, we can increment their value by at-most one. This
		// correspond to values that where rejected by having the same slugs as
		// prioritized objects.
		i++
		for ; i < l; i++ {
			if c := offsets[i]; c > 0 && cursors[i] == c-1 {
				cursors[i] = c
			}
		}
	}

	for i, reg := range a.registries {
		if e := reg.ended; e >= 0 && cursors[i] >= e {
			cursors[i] = -1
		}
	}

	// Advance lower-priority dimensions over entries whose slug is masked by
	// a higher-priority registry.
	for i := 1; i < l; i++ {
		reg := a.registries[i]
		for cursors[i] >= 0 && reg.maskedOffsets[cursors[i]] {
			cursors[i]++
		}
		if e := reg.ended; e >= 0 && cursors[i] >= e {
			cursors[i] = -1
		}
	}

	if len(list) == 0 {
		for i, reg := range a.registries {
			if reg.ended >= 0 {
				cursors[i] = -1
			}
		}
	}

	return &AppsPaginated{
		Apps: list,
		PageInfo: PageInfo{
			Count:      len(list),
			NextCursor: printMutliCursor(cursors),
		},
	}
}

func fetchUntilFound(client *http.Client, registries []*url.URL, requestURI string, cache CacheControl) (resp *http.Response, ok bool, err error) {
	ref, err := url.Parse(requestURI)
	if err != nil {
		return
	}
	for _, registry := range registries {
		resp, ok, err = fetch(client, registry, ref, cache)
		if err != nil {
			return
		}
		if !ok {
			continue
		}
		return
	}
	return nil, false, nil
}

func fetch(client *http.Client, registry, ref *url.URL, cache CacheControl) (resp *http.Response, ok bool, err error) {
	u := registry.ResolveReference(ref)
	u.Path = path.Join(registry.Path, ref.Path)
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return
	}
	if cache == NoCache {
		req.Header.Set("cache-control", "no-cache")
	}
	start := time.Now()
	resp, err = client.Do(req)
	if err != nil {
		return
	}
	elapsed := time.Since(start)
	defer func() {
		if !ok {
			// Flush the body, so that the connection can be reused by keep-alive
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	if elapsed.Seconds() >= 3 {
		log := logger.WithNamespace("registry")
		log.Infof("slow request on %s (%s)", u.String(), elapsed)
	}
	if resp.StatusCode == 404 {
		return
	}
	if resp.StatusCode != 200 {
		var msg struct {
			Message string `json:"message"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&msg); err != nil {
			err = echo.NewHTTPError(resp.StatusCode)
		} else {
			err = echo.NewHTTPError(resp.StatusCode, msg.Message)
		}
		return
	}
	return resp, true, nil
}

func printMutliCursor(c []int) string {
	// if all dimensions of the multi-cursor are -1, we print the empty string
	sum := 0
	for _, i := range c {
		sum += i
	}
	if sum == -len(c) {
		return ""
	}
	var a []string
	for _, i := range c {
		a = append(a, strconv.Itoa(i))
	}
	return strings.Join(a, "|")
}

func removeQueries(u *url.URL, filter ...string) *url.URL {
	u, _ = url.Parse(u.String())
	q1 := u.Query()
	q2 := make(url.Values)
	for k, v := range q1 {
		if len(v) == 0 {
			continue
		}
		var remove bool
		for _, f := range filter {
			if f == k {
				remove = true
				break
			}
		}
		if !remove {
			q2.Add(k, v[0])
		}
	}
	u.RawQuery = q2.Encode()
	return u
}

func addQueries(u *url.URL, queries ...string) *url.URL {
	u, _ = url.Parse(u.String())
	q := u.Query()
	for i := 0; i < len(queries); i += 2 {
		q.Add(queries[i], queries[i+1])
	}
	u.RawQuery = q.Encode()
	return u
}
