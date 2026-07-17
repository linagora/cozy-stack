// Package instances is used for the admin endpoint to manage instances. It
// covers a lot of things, from creating an instance to checking the FS
// integrity.
package instances

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/instance/storagemigration"
	"github.com/cozy/cozy-stack/model/notification"
	"github.com/cozy/cozy-stack/model/notification/center"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/labstack/echo/v4"
)

type apiInstance struct {
	*instance.Instance
}

func (i *apiInstance) MarshalJSON() ([]byte, error) {
	return json.Marshal(i.Instance)
}

// Links is used to generate a JSON-API link for the instance
func (i *apiInstance) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/instances/" + i.Instance.DocID}
}

// Relationships is used to generate the content relationship in JSON-API format
func (i *apiInstance) Relationships() jsonapi.RelationshipMap {
	return jsonapi.RelationshipMap{}
}

// Included is part of the jsonapi.Object interface
func (i *apiInstance) Included() []jsonapi.Object {
	return nil
}

func createHandler(c echo.Context) error {
	var err error
	opts := &lifecycle.Options{
		Domain:          c.QueryParam("Domain"),
		OrgDomain:       c.QueryParam("OrgDomain"),
		OrgID:           c.QueryParam("OrgID"),
		OldDomain:       c.QueryParam("OldDomain"),
		Locale:          c.QueryParam("Locale"),
		UUID:            c.QueryParam("UUID"),
		OIDCID:          c.QueryParam("OIDCID"),
		FranceConnectID: c.QueryParam("FranceConnectID"),
		TOSSigned:       c.QueryParam("TOSSigned"),
		TOSLatest:       c.QueryParam("TOSLatest"),
		Timezone:        c.QueryParam("Timezone"),
		ContextName:     c.QueryParam("ContextName"),
		Email:           c.QueryParam("Email"),
		PublicName:      c.QueryParam("PublicName"),
		Phone:           c.QueryParam("Phone"),
		Settings:        c.QueryParam("Settings"),
		AuthMode:        c.QueryParam("AuthMode"),
		Passphrase:      c.QueryParam("Passphrase"),
		Key:             c.QueryParam("Key"),
		Apps:            utils.SplitTrimString(c.QueryParam("Apps"), ","),
	}
	if domainAliases := c.QueryParam("DomainAliases"); domainAliases != "" {
		opts.DomainAliases = strings.Split(domainAliases, ",")
	}
	if sponsorships := c.QueryParam("sponsorships"); sponsorships != "" {
		opts.Sponsorships = strings.Split(sponsorships, ",")
	}
	if featureSets := c.QueryParam("feature_sets"); featureSets != "" {
		opts.FeatureSets = strings.Split(featureSets, ",")
	}
	if autoUpdate := c.QueryParam("AutoUpdate"); autoUpdate != "" {
		b, err := strconv.ParseBool(autoUpdate)
		if err != nil {
			return wrapError(err)
		}
		opts.AutoUpdate = &b
	}
	if magicLink := c.QueryParam("MagicLink"); magicLink != "" {
		ml, err := strconv.ParseBool(magicLink)
		if err != nil {
			return wrapError(err)
		}
		opts.MagicLink = &ml
	}
	if layout := c.QueryParam("SwiftLayout"); layout != "" {
		opts.SwiftLayout, err = strconv.Atoi(layout)
		if err != nil {
			return wrapError(err)
		}
	} else {
		opts.SwiftLayout = -1
	}
	if cluster := c.QueryParam("CouchCluster"); cluster != "" {
		opts.CouchCluster, err = strconv.Atoi(cluster)
		if err != nil {
			return wrapError(err)
		}
	} else {
		opts.CouchCluster = -1
	}
	if diskQuota := c.QueryParam("DiskQuota"); diskQuota != "" {
		opts.DiskQuota, err = strconv.ParseInt(diskQuota, 10, 64)
		if err != nil {
			return wrapError(err)
		}
	}
	if iterations := c.QueryParam("KdfIterations"); iterations != "" {
		iter, err := strconv.Atoi(iterations)
		if err != nil {
			return wrapError(err)
		}
		if iter < crypto.MinPBKDF2Iterations && iter != 0 {
			err := errors.New("The KdfIterations number is too low")
			return jsonapi.InvalidParameter("KdfIterations", err)
		}
		if iter > crypto.MaxPBKDF2Iterations {
			err := errors.New("The KdfIterations number is too high")
			return jsonapi.InvalidParameter("KdfIterations", err)
		}
		opts.KdfIterations = iter
	}
	if traced, err := strconv.ParseBool(c.QueryParam("Trace")); err == nil {
		opts.Traced = &traced
	}
	in, err := lifecycle.Create(opts)
	if err != nil {
		return wrapError(err)
	}
	in.CLISecret = nil
	in.OAuthSecret = nil
	in.SessSecret = nil
	in.PassphraseHash = nil
	return jsonapi.Data(c, http.StatusCreated, &apiInstance{in}, nil)
}

func showHandler(c echo.Context) error {
	domain := c.Param("domain")
	in, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}
	in.CLISecret = nil
	in.OAuthSecret = nil
	in.SessSecret = nil
	in.PassphraseHash = nil
	return jsonapi.Data(c, http.StatusOK, &apiInstance{in}, nil)
}

func modifyHandler(c echo.Context) error {
	domain := c.Param("domain")
	opts := &lifecycle.Options{
		Domain:          domain,
		OldDomain:       c.QueryParam("OldDomain"),
		Locale:          c.QueryParam("Locale"),
		UUID:            c.QueryParam("UUID"),
		OIDCID:          c.QueryParam("OIDCID"),
		FranceConnectID: c.QueryParam("FranceConnectID"),
		TOSSigned:       c.QueryParam("TOSSigned"),
		TOSLatest:       c.QueryParam("TOSLatest"),
		Timezone:        c.QueryParam("Timezone"),
		ContextName:     c.QueryParam("ContextName"),
		Email:           c.QueryParam("Email"),
		PublicName:      c.QueryParam("PublicName"),
		Phone:           c.QueryParam("Phone"),
		Settings:        c.QueryParam("Settings"),
		BlockingReason:  c.QueryParam("BlockingReason"),
	}
	if domainAliases := c.QueryParam("DomainAliases"); domainAliases != "" {
		opts.DomainAliases = strings.Split(domainAliases, ",")
	}
	if sponsorships := c.QueryParam("Sponsorships"); sponsorships != "" {
		opts.Sponsorships = strings.Split(sponsorships, ",")
	}
	if quota := c.QueryParam("DiskQuota"); quota != "" {
		i, err := strconv.ParseInt(quota, 10, 64)
		if err != nil {
			return wrapError(err)
		}
		opts.DiskQuota = i
	}
	if onboardingFinished, err := strconv.ParseBool(c.QueryParam("OnboardingFinished")); err == nil {
		opts.OnboardingFinished = &onboardingFinished
	}
	if magicLink, err := strconv.ParseBool(c.QueryParam("MagicLink")); err == nil {
		opts.MagicLink = &magicLink
	}
	// Deprecated: the Debug parameter should no longer be used, but is kept
	// for compatibility.
	if debug, err := strconv.ParseBool(c.QueryParam("Debug")); err == nil {
		opts.Debug = &debug
	}
	if blocked, err := strconv.ParseBool(c.QueryParam("Blocked")); err == nil {
		opts.Blocked = &blocked
	}
	if from, err := strconv.ParseBool(c.QueryParam("FromCloudery")); err == nil {
		opts.FromCloudery = from
	}
	i, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}
	// XXX we cannot use the lifecycle.Patch function to update the deleting
	// flag, as we may need to update this flag for an instance that no longer
	// has its settings database.
	if deleting, err := strconv.ParseBool(c.QueryParam("Deleting")); err == nil {
		i.Deleting = deleting
		if err := instance.Update(i); err != nil {
			return wrapError(err)
		}
		return jsonapi.Data(c, http.StatusOK, &apiInstance{i}, nil)
	}
	if err = lifecycle.Patch(i, opts); err != nil {
		return wrapError(err)
	}
	return jsonapi.Data(c, http.StatusOK, &apiInstance{i}, nil)
}

func listHandler(c echo.Context) error {
	var instances []*instance.Instance
	var links *jsonapi.LinksList
	var err error

	var limit int
	if l := c.QueryParam("page[limit]"); l != "" {
		if converted, err := strconv.Atoi(l); err == nil {
			limit = converted
		}
	}

	var skip int
	if s := c.QueryParam("page[skip]"); s != "" {
		if converted, err := strconv.Atoi(s); err == nil {
			skip = converted
		}
	}

	if limit > 0 {
		cursor := c.QueryParam("page[cursor]")
		instances, cursor, err = instance.PaginatedList(limit, cursor, skip)
		if cursor != "" {
			links = &jsonapi.LinksList{
				Next: fmt.Sprintf("/instances?page[limit]=%d&page[cursor]=%s", limit, cursor),
			}
		}
	} else {
		instances, err = instance.List()
	}
	if err != nil {
		if couchdb.IsNoDatabaseError(err) {
			return jsonapi.DataList(c, http.StatusOK, nil, nil)
		}
		return wrapError(err)
	}

	objs := make([]jsonapi.Object, len(instances))
	for i, in := range instances {
		in.CLISecret = nil
		in.OAuthSecret = nil
		in.SessSecret = nil
		in.PassphraseHash = nil
		objs[i] = &apiInstance{in}
	}

	return jsonapi.DataList(c, http.StatusOK, objs, links)
}

func countHandler(c echo.Context) error {
	count, err := couchdb.CountNormalDocs(prefixer.GlobalPrefixer, consts.Instances)
	if couchdb.IsNoDatabaseError(err) {
		count = 0
	} else if err != nil {
		return wrapError(err)
	}
	return c.JSON(http.StatusOK, echo.Map{"count": count})
}

func deleteHandler(c echo.Context) error {
	domain := c.Param("domain")
	err := lifecycle.Destroy(domain)
	if err != nil {
		return wrapError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func migrateStorageHandler(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return wrapError(err)
	}
	opts := storagemigration.Options{
		To:          c.QueryParam("to"),
		DryRun:      c.QueryParam("dry_run") == "true",
		FlagOnly:    c.QueryParam("flag_only") == "true",
		Force:       c.QueryParam("force") == "true",
		PurgeSource: c.QueryParam("purge_source") == "true",
	}
	rep, err := storagemigration.Migrate(inst, opts)
	if err != nil {
		return wrapError(err)
	}
	return c.JSON(http.StatusOK, rep)
}

func setAuthMode(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}
	m := echo.Map{}
	if err := c.Bind(&m); err != nil {
		return err
	}

	authModeString, ok := m["auth_mode"]
	if !ok {
		return jsonapi.BadRequest(errors.New("Missing auth_mode key"))
	}

	authMode, err := instance.StringToAuthMode(authModeString.(string))
	if err != nil {
		return jsonapi.BadRequest(err)
	}

	if !inst.HasAuthMode(authMode) {
		inst.AuthMode = authMode
		if err = instance.Update(inst); err != nil {
			return err
		}
	} else {
		alreadyAuthMode := fmt.Sprintf("Instance has already %s auth mode", authModeString)
		return c.JSON(http.StatusOK, alreadyAuthMode)
	}
	// Return success
	return c.JSON(http.StatusNoContent, nil)
}

func createMagicLink(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	code, err := lifecycle.CreateMagicLinkCode(inst)
	if err != nil {
		if err == lifecycle.ErrMagicLinkNotAvailable {
			return c.JSON(http.StatusBadRequest, echo.Map{
				"error": err,
			})
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err,
		})
	}

	req := c.Request()
	var ip string
	if forwardedFor := req.Header.Get(echo.HeaderXForwardedFor); forwardedFor != "" {
		ip = strings.TrimSpace(strings.SplitN(forwardedFor, ",", 2)[0])
	}
	if ip == "" {
		ip = strings.Split(req.RemoteAddr, ":")[0]
	}
	inst.Logger().WithField("nspace", "loginaudit").
		Infof("New magic_link code created from %s at %s", ip, time.Now())

	return c.JSON(http.StatusCreated, echo.Map{
		"code": code,
	})
}

func createSessionCode(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	code, err := inst.CreateSessionCode()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err,
		})
	}

	req := c.Request()
	var ip string
	if forwardedFor := req.Header.Get(echo.HeaderXForwardedFor); forwardedFor != "" {
		ip = strings.TrimSpace(strings.SplitN(forwardedFor, ",", 2)[0])
	}
	if ip == "" {
		ip = strings.Split(req.RemoteAddr, ":")[0]
	}
	inst.Logger().WithField("nspace", "loginaudit").
		Infof("New session_code created from %s at %s", ip, time.Now())

	return c.JSON(http.StatusCreated, echo.Map{
		"session_code": code,
	})
}

type checkSessionCodeArgs struct {
	Code string `json:"session_code"`
}

func checkSessionCode(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	var args checkSessionCodeArgs
	if err := c.Bind(&args); err != nil {
		return err
	}

	ok := inst.CheckAndClearSessionCode(args.Code)
	if !ok {
		return c.JSON(http.StatusForbidden, echo.Map{"valid": false})
	}

	return c.JSON(http.StatusOK, echo.Map{"valid": true})
}

func createEmailVerifiedCode(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	if !inst.HasAuthMode(instance.TwoFactorMail) {
		return jsonapi.BadRequest(errors.New("2FA by email is not enabled on this instance"))
	}

	code, err := inst.CreateEmailVerifiedCode()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err,
		})
	}

	req := c.Request()
	var ip string
	if forwardedFor := req.Header.Get(echo.HeaderXForwardedFor); forwardedFor != "" {
		ip = strings.TrimSpace(strings.SplitN(forwardedFor, ",", 2)[0])
	}
	if ip == "" {
		ip = strings.Split(req.RemoteAddr, ":")[0]
	}
	inst.Logger().WithField("nspace", "loginaudit").
		Infof("New email_verified_code created from %s at %s", ip, time.Now())

	return c.JSON(http.StatusCreated, echo.Map{
		"email_verified_code": code,
	})
}

func cleanSessions(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	if err := couchdb.DeleteDB(inst, consts.Sessions); err != nil && !couchdb.IsNoDatabaseError(err) {
		return err
	}
	if err := couchdb.DeleteDB(inst, consts.SessionsLogins); err != nil && !couchdb.IsNoDatabaseError(err) {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func lastActivity(c echo.Context) error {
	inst, err := instance.Get(c.Param("domain"))
	if err != nil {
		return jsonapi.NotFound(err)
	}
	last := time.Date(2018, time.January, 1, 0, 0, 0, 0, time.UTC)
	if inst.LastActivityFromDeletedOAuthClients != nil {
		last = *inst.LastActivityFromDeletedOAuthClients
	}

	err = couchdb.ForeachDocs(inst, consts.SessionsLogins, func(_ string, data json.RawMessage) error {
		var entry session.LoginEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			return err
		}
		if last.Before(entry.CreatedAt) {
			last = entry.CreatedAt
		}
		return nil
	})
	if err != nil {
		return err
	}

	err = couchdb.ForeachDocs(inst, consts.Sessions, func(_ string, data json.RawMessage) error {
		var sess session.Session
		if err := json.Unmarshal(data, &sess); err != nil {
			return err
		}
		if last.Before(sess.LastSeen) {
			last = sess.LastSeen
		}
		return nil
	})
	// If the instance has not yet been onboarded, the io.cozy.sessions
	// database will not exist.
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return err
	}

	err = couchdb.ForeachDocs(inst, consts.OAuthClients, func(_ string, data json.RawMessage) error {
		var client oauth.Client
		if err := json.Unmarshal(data, &client); err != nil {
			return err
		}
		// Ignore the OAuth clients used for sharings
		if client.ClientKind == "sharing" {
			return nil
		}
		if at, ok := client.LastRefreshedAt.(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
				if last.Before(t) {
					last = t
				}
			}
		}
		if at, ok := client.SynchronizedAt.(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
				if last.Before(t) {
					last = t
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, echo.Map{
		"last-activity": last.Format("2006-01-02"),
	})
}

func unxorID(c echo.Context) error {
	inst, err := instance.Get(c.Param("domain"))
	if err != nil {
		return jsonapi.NotFound(err)
	}
	s, err := sharing.FindSharing(inst, c.Param("sharing-id"))
	if err != nil {
		return jsonapi.NotFound(err)
	}
	if s.Owner {
		err := errors.New("it only works on a recipient's instance")
		return jsonapi.BadRequest(err)
	}
	if len(s.Credentials) != 1 || len(s.Credentials[0].XorKey) == 0 {
		err := errors.New("unexpected credentials")
		return jsonapi.BadRequest(err)
	}
	key := s.Credentials[0].XorKey
	id := sharing.XorID(c.Param("doc-id"), key)
	return c.JSON(http.StatusOK, echo.Map{"id": id})
}

type diskUsageResult struct {
	Used          int64 `json:"used,string"`
	Quota         int64 `json:"quota,string,omitempty"`
	Count         int   `json:"doc_count,omitempty"`
	Files         int64 `json:"files,string,omitempty"`
	Versions      int64 `json:"versions,string,omitempty"`
	VersionsCount int   `json:"versions_count,string,omitempty"`
	Trashed       int64 `json:"trashed,string,omitempty"`
}

func diskUsage(c echo.Context) error {
	domain := c.Param("domain")
	instance, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}
	fs := instance.VFS()

	files, err := fs.FilesUsage()
	if err != nil {
		return err
	}

	versions, err := fs.VersionsUsage()
	if err != nil {
		return err
	}

	result := &diskUsageResult{}
	result.Used = files + versions
	result.Files = files
	result.Versions = versions

	if c.QueryParam("include") == "trash" {
		trashed, err := fs.TrashUsage()
		if err != nil {
			return err
		}
		result.Trashed = trashed
	}

	result.Quota = fs.DiskQuota()
	if stats, err := couchdb.DBStatus(instance, consts.Files); err == nil {
		result.Count = stats.DocCount
	}
	if stats, err := couchdb.DBStatus(instance, consts.FilesVersions); err == nil {
		result.VersionsCount = stats.DocCount
	}
	return c.JSON(http.StatusOK, result)
}

func sendNotification(c echo.Context) error {
	domain := c.Param("domain")
	instance, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	m := map[string]json.RawMessage{}
	if err := json.NewDecoder(c.Request().Body).Decode(&m); err != nil {
		return err
	}

	p := &notification.Properties{}
	if err := json.Unmarshal(m["properties"], &p); err != nil {
		return err
	}

	n := &notification.Notification{}
	if err := json.Unmarshal(m["notification"], &n); err != nil {
		return err
	}

	if err := center.PushCLI(instance.DomainName(), p, n); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, n)
}

func showPrefix(c echo.Context) error {
	domain := c.Param("domain")

	instance, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, instance.DBPrefix())
}

func getSwiftBucketName(c echo.Context) error {
	domain := c.Param("domain")

	instance, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	var containerNames map[string]string
	type swifter interface {
		ContainerNames() map[string]string
	}
	if obj, ok := instance.VFS().(swifter); ok {
		containerNames = obj.ContainerNames()
	}

	return c.JSON(http.StatusOK, containerNames)
}

func appVersion(c echo.Context) error {
	instances, err := instance.List()
	if err != nil {
		return nil
	}
	appSlug := c.Param("slug")
	version := c.Param("version")

	var instancesAppVersion []string

	for _, instance := range instances {
		app, err := app.GetBySlug(instance, appSlug, consts.WebappType)
		if err == nil {
			if app.Version() == version {
				instancesAppVersion = append(instancesAppVersion, instance.Domain)
			}
		}
	}

	i := struct {
		Instances []string `json:"instances"`
	}{
		instancesAppVersion,
	}

	return c.JSON(http.StatusOK, i)
}

func wrapError(err error) error {
	switch err {
	case instance.ErrNotFound:
		return jsonapi.NotFound(err)
	case instance.ErrExists:
		return jsonapi.Conflict(err)
	case instance.ErrIllegalDomain:
		return jsonapi.InvalidParameter("domain", err)
	case instance.ErrMissingToken:
		return jsonapi.BadRequest(err)
	case instance.ErrInvalidToken:
		return jsonapi.BadRequest(err)
	case instance.ErrMissingPassphrase:
		return jsonapi.BadRequest(err)
	case instance.ErrInvalidPassphrase:
		return jsonapi.BadRequest(err)
	case instance.ErrBadTOSVersion:
		return jsonapi.BadRequest(err)
	}
	return err
}

// Routes sets the routing for the instances service
func Routes(router *echo.Group) {
	// CRUD for instances
	router.GET("", listHandler)
	router.POST("", createHandler)
	router.GET("/count", countHandler)
	router.GET("/:domain", showHandler)
	router.PATCH("/:domain", modifyHandler)
	router.DELETE("/:domain", deleteHandler)

	// Debug mode
	router.GET("/:domain/debug", getDebug)
	router.POST("/:domain/debug", enableDebug)
	router.DELETE("/:domain/debug", disableDebug)

	// Feature flags
	router.GET("/:domain/feature/flags", getFeatureFlags)
	router.PATCH("/:domain/feature/flags", patchFeatureFlags)
	router.GET("/:domain/feature/sets", getFeatureSets)
	router.PUT("/:domain/feature/sets", putFeatureSets)
	router.GET("/feature/config/:context", getFeatureConfig)
	router.GET("/feature/contexts/:context", getFeatureContext)
	router.PATCH("/feature/contexts/:context", patchFeatureContext)
	router.GET("/feature/defaults", getFeatureDefaults)
	router.PATCH("/feature/defaults", patchFeatureDefaults)

	// Authentication
	router.POST("/token", createToken)
	router.GET("/oauth_client", findClientBySoftwareID)
	router.POST("/oauth_client", registerClient)
	router.POST("/:domain/auth-mode", setAuthMode)
	router.POST("/:domain/magic_link", createMagicLink)
	router.POST("/:domain/session_code", createSessionCode)
	router.POST("/:domain/session_code/check", checkSessionCode)
	router.POST("/:domain/email_verified_code", createEmailVerifiedCode)
	router.DELETE("/:domain/sessions", cleanSessions)

	// Advanced features for instances
	router.GET("/:domain/last-activity", lastActivity)
	router.POST("/:domain/export", exporter)
	router.GET("/:domain/exports/:export-id/data", dataExporter)
	router.POST("/:domain/import", importer)
	router.GET("/:domain/disk-usage", diskUsage)
	router.GET("/:domain/prefix", showPrefix)
	router.GET("/:domain/swift-prefix", getSwiftBucketName)
	router.GET("/:domain/sharings/:sharing-id/unxor/:doc-id", unxorID)
	router.POST("/:domain/notifications", sendNotification)

	// Config
	router.POST("/redis", rebuildRedis)
	router.GET("/assets", assetsInfos)
	router.POST("/assets", addAssets)
	router.DELETE("/assets/:context/*", deleteAssets)
	router.GET("/contexts", lsContexts)
	router.GET("/contexts/:name", showContext)
	router.GET("/with-app-version/:slug/:version", appVersion)

	// Storage migration
	router.POST("/:domain/migrate-storage", migrateStorageHandler)

	// Checks
	router.GET("/:domain/fsck", fsckHandler)
	router.POST("/:domain/checks/triggers", checkTriggers)
	router.POST("/:domain/checks/shared", checkShared)
	router.POST("/:domain/checks/sharings", checkSharings)

	// Fixers
	router.POST("/:domain/fixers/password-defined", passwordDefinedFixer)
	router.POST("/:domain/fixers/orphan-account", orphanAccountFixer)
	router.POST("/:domain/fixers/sharings-moved", sharingsMovedFixer)
	router.POST("/:domain/fixers/service-triggers", serviceTriggersFixer)
	router.POST("/:domain/fixers/indexes", indexesFixer)
}
