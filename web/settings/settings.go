// Package settings regroups some API methods to facilitate the usage of the
// io.cozy settings documents.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/session"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/model/settings/common"
	"github.com/cozy/cozy-stack/model/token"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
	"github.com/mssola/user_agent"
)

type apiSession struct {
	s *session.Session
}

func (s *apiSession) ID() string                             { return s.s.ID() }
func (s *apiSession) Rev() string                            { return s.s.Rev() }
func (s *apiSession) DocType() string                        { return consts.Sessions }
func (s *apiSession) Clone() couchdb.Doc                     { return s }
func (s *apiSession) SetID(_ string)                         {}
func (s *apiSession) SetRev(_ string)                        {}
func (s *apiSession) Relationships() jsonapi.RelationshipMap { return nil }
func (s *apiSession) Included() []jsonapi.Object             { return nil }
func (s *apiSession) Links() *jsonapi.LinksList              { return nil }
func (s *apiSession) MarshalJSON() ([]byte, error)           { return json.Marshal(s.s) }

// HTTPHandler handle all the `/settings` routes.
type HTTPHandler struct {
	svc csettings.Service
	rmq rabbitmq.Service
}

// NewHTTPHandler instantiates a new [HTTPHandler].
func NewHTTPHandler(svc csettings.Service, rmq rabbitmq.Service) *HTTPHandler {
	return &HTTPHandler{svc: svc, rmq: rmq}
}

func (h *HTTPHandler) getSessions(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	if err := middlewares.AllowWholeType(c, permission.GET, consts.Sessions); err != nil {
		return err
	}

	sessions, err := session.GetAll(inst)
	if err != nil {
		return err
	}

	objs := make([]jsonapi.Object, len(sessions))
	for i, s := range sessions {
		objs[i] = &apiSession{s}
	}

	return jsonapi.DataList(c, http.StatusOK, objs, nil)
}

func (h *HTTPHandler) getCurrentSession(c echo.Context) error {
	sess, ok := middlewares.GetSession(c)
	if !ok {
		return jsonapi.NotFound(errors.New("no current session"))
	}
	return jsonapi.Data(c, http.StatusOK, &apiSession{sess}, nil)
}

func (h *HTTPHandler) listWarnings(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	// Any request with a token can ask for the context (no permissions are required)
	if _, err := middlewares.GetPermission(c); err != nil && !isMovedError(err) {
		return err
	}

	w := middlewares.ListWarnings(inst)

	if len(w) == 0 {
		// Sends a 404 when there is no warnings
		resp := c.Response()
		resp.Header().Set(echo.HeaderContentType, jsonapi.ContentType)
		resp.WriteHeader(http.StatusNotFound)
		_, err := resp.Write([]byte("{\"errors\": []}"))
		return err
	}

	return jsonapi.DataErrorList(c, w...)
}

// postEmail handle POST /settings/email
func (h *HTTPHandler) postEmail(c echo.Context) error {
	type body struct {
		Passphrase string `json:"passphrase"`
		Email      string `json:"email"`
	}

	if err := middlewares.AllowWholeType(c, permission.POST, consts.Settings); err != nil {
		return err
	}

	var args body
	err := c.Bind(&args)
	if err != nil {
		return jsonapi.BadJSON()
	}

	inst := middlewares.GetInstance(c)

	// On signup-managed contexts the email is owned by the external signup flow,
	// so it cannot be changed through the stack.
	if inst.HasSignup() {
		return jsonapi.NewError(http.StatusForbidden, "email is managed externally and cannot be changed for this instance")
	}

	err = h.svc.StartEmailUpdate(inst, &csettings.UpdateEmailCmd{
		Passphrase: []byte(args.Passphrase),
		Email:      args.Email,
	})

	switch {
	case err == nil:
		c.NoContent(http.StatusNoContent)
		return nil
	case errors.Is(err, instance.ErrInvalidPassphrase):
		return jsonapi.BadRequest(instance.ErrInvalidPassphrase)
	default:
		return jsonapi.InternalServerError(err)
	}
}

// postEmailResend handle POST /settings/email/resend
func (h *HTTPHandler) postEmailResend(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.POST, consts.Settings); err != nil {
		return err
	}

	inst := middlewares.GetInstance(c)

	err := h.svc.ResendEmailUpdate(inst)

	switch {
	case err == nil:
		c.NoContent(http.StatusNoContent)
		return nil
	case errors.Is(err, instance.ErrInvalidPassphrase):
		return jsonapi.BadRequest(instance.ErrInvalidPassphrase)
	default:
		return jsonapi.InternalServerError(err)
	}
}

// deleteEmail handle DELETE /settings/email
func (h *HTTPHandler) deleteEmail(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.POST, consts.Settings); err != nil {
		return err
	}

	inst := middlewares.GetInstance(c)

	err := h.svc.CancelEmailUpdate(inst)
	switch {
	case err == nil:
		c.NoContent(http.StatusNoContent)
		return nil
	case errors.Is(err, csettings.ErrNoPendingEmail):
		return jsonapi.BadRequest(csettings.ErrNoPendingEmail)
	default:
		return jsonapi.InternalServerError(err)
	}
}

func (h *HTTPHandler) getEmailConfirmation(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if !middlewares.IsLoggedIn(c) {
		u := inst.PageURL("/auth/login", url.Values{
			"redirect": {inst.FromURL(c.Request().URL)},
		})
		return c.Redirect(http.StatusSeeOther, u)
	}

	tok := c.QueryParam("token")
	settingsURL := inst.SubDomain("settings").String()

	err := h.svc.ConfirmEmailUpdate(inst, tok)
	switch {
	case err == nil:
		// Redirect to the setting page
		return c.Redirect(http.StatusTemporaryRedirect, settingsURL)
	case errors.Is(err, csettings.ErrNoPendingEmail), errors.Is(err, token.ErrInvalidToken):
		return c.Render(http.StatusBadRequest, "error.html", echo.Map{
			"Domain":         inst.ContextualDomain(),
			"ContextName":    inst.ContextName,
			"Locale":         inst.Locale,
			"Title":          inst.TemplateTitle(),
			"Favicon":        middlewares.Favicon(inst),
			"Illustration":   "/images/generic-error.svg",
			"ErrorTitle":     "Error InvalidToken Title",
			"Error":          "Error InvalidToken Message",
			"Link":           "Error InvalidToken Link",
			"LinkURL":        settingsURL,
			"SupportEmail":   inst.SupportEmailAddress(),
			"SupportPageURL": inst.SupportPageURL(),
		})
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, err)
	}
}

func (h *HTTPHandler) installFlagshipApp(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	rawUserAgent := c.Request().UserAgent()
	ua := user_agent.New(rawUserAgent)
	platform := strings.ToLower(ua.Platform())
	os := strings.ToLower(ua.OS())
	flags, err := feature.GetFlags(inst)
	if err != nil {
		return err
	}

	storeLink := "https://cozy.io/" + inst.Locale + "/download"
	if strings.Contains(platform, "iphone") || strings.Contains(platform, "ipad") {
		id, ok := flags.M["flagship.appstore_id"].(string)
		if !ok {
			id = "id1600636174"
		}
		storeLink = fmt.Sprintf("https://apps.apple.com/%s/app/%s", inst.Locale, id)
	} else if strings.Contains(platform, "android") || strings.Contains(os, "android") {
		id, ok := flags.M["flagship.playstore_id"].(string)
		if !ok {
			id = "io.cozy.flagship.mobile"
		}
		storeLink = fmt.Sprintf("https://play.google.com/store/apps/details?id=%s&hl=%s", id, inst.Locale)
	}

	return c.Render(http.StatusOK, "install_flagship_app.html", echo.Map{
		"Domain":      inst.ContextualDomain(),
		"ContextName": inst.ContextName,
		"Locale":      inst.Locale,
		"Title":       inst.TemplateTitle(),
		"Favicon":     middlewares.Favicon(inst),
		"StoreLink":   storeLink,
		"SkipLink":    inst.OnboardedRedirection().String(),
	})
}

func isMovedError(err error) bool {
	j, ok := err.(*jsonapi.Error)
	return ok && j.Code == "moved"
}

func (h *HTTPHandler) UploadAvatar(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, http.MethodPut, consts.Settings); err != nil {
		return err
	}
	header := c.Request().Header
	size := c.Request().ContentLength
	if size > 20_000_000 {
		return jsonapi.Errorf(http.StatusRequestEntityTooLarge, "Avatar is too big")
	}
	contentType := header.Get(echo.HeaderContentType)
	f, err := inst.AvatarFS().CreateAvatar(contentType)
	if err != nil {
		return jsonapi.InternalServerError(err)
	}
	_, err = io.Copy(f, c.Request().Body)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return jsonapi.InternalServerError(err)
	}

	updated, err := common.UpdateAvatar(inst)
	if err != nil {
		inst.Logger().Errorf("Failed to update common settings: %s", err)
	}
	if updated {
		// We need to persist the common settings version
		if err = instance.Update(inst); err != nil {
			return err
		}
	}

	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) DeleteAvatar(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, http.MethodPut, consts.Settings); err != nil {
		return err
	}
	err := inst.AvatarFS().DeleteAvatar()
	if err != nil {
		return jsonapi.InternalServerError(err)
	}
	// Bump the common settings version and republish the avatar URL to bust caches
	updated, uerr := common.UpdateAvatar(inst)
	if uerr != nil {
		inst.Logger().Errorf("Failed to update common settings: %s", uerr)
	}
	if updated {
		// We need to persist the common settings version
		if uerr = instance.Update(inst); uerr != nil {
			return uerr
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// Register all the `/settings` routes to the given router.
func (h *HTTPHandler) Register(router *echo.Group) {
	router.GET("/disk-usage", h.diskUsage)
	router.GET("/clients-usage", h.clientsUsage)

	router.POST("/email", h.postEmail)
	router.POST("/email/resend", h.postEmailResend)
	router.DELETE("/email", h.deleteEmail)
	router.GET("/email/confirm", h.getEmailConfirmation)

	router.GET("/passphrase", h.getPassphraseParameters)
	router.POST("/passphrase", h.registerPassphrase)
	router.POST("/passphrase/flagship", h.registerPassphraseFlagship)
	router.PUT("/passphrase", h.updatePassphrase)
	router.POST("/passphrase/check", h.checkPassphrase)
	router.GET("/hint", h.getHint)
	router.PUT("/hint", h.updateHint)
	router.POST("/vault", h.createVault)

	router.GET("/capabilities", h.getCapabilities)
	router.GET("/external-ties", h.getExternalTies)
	router.GET("/premium", h.redirectToPremium)
	router.GET("/instance", h.getInstance)
	router.PUT("/instance", h.updateInstance)
	router.POST("/instance/deletion", h.askInstanceDeletion)
	router.POST("/instance/deletion/force", h.forceInstanceDeletion)
	router.PUT("/instance/auth_mode", h.updateInstanceAuthMode)
	router.PUT("/instance/sign_tos", h.updateInstanceTOS)
	router.DELETE("/instance/moved_from", h.clearMovedFrom)

	router.PUT("/avatar", h.UploadAvatar)
	router.DELETE("/avatar", h.DeleteAvatar)

	router.GET("/flags", h.getFlags)

	router.GET("/sessions", h.getSessions)
	router.GET("/sessions/current", h.getCurrentSession)

	router.GET("/clients", h.listClients)
	router.DELETE("/clients/:id", h.revokeClient)
	router.GET("/clients/limit-exceeded", h.limitExceeded)
	router.POST("/synchronized", h.synchronized)

	router.GET("/onboarded", h.onboarded)
	router.GET("/install_flagship_app", h.installFlagshipApp)
	router.GET("/context", h.context)
	router.GET("/warnings", h.listWarnings)
}
