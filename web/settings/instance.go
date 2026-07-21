// Package settings regroups some API methods to facilitate the usage of the
// io.cozy settings documents.
package settings

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

type apiInstance struct {
	doc *couchdb.JSONDoc
}

func (i *apiInstance) ID() string                             { return i.doc.ID() }
func (i *apiInstance) Rev() string                            { return i.doc.Rev() }
func (i *apiInstance) DocType() string                        { return consts.Settings }
func (i *apiInstance) Clone() couchdb.Doc                     { return i }
func (i *apiInstance) SetID(id string)                        { i.doc.SetID(id) }
func (i *apiInstance) SetRev(rev string)                      { i.doc.SetRev(rev) }
func (i *apiInstance) Relationships() jsonapi.RelationshipMap { return nil }
func (i *apiInstance) Included() []jsonapi.Object             { return nil }
func (i *apiInstance) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/settings/instance"}
}

func (i *apiInstance) MarshalJSON() ([]byte, error) {
	return json.Marshal(i.doc)
}

func (h *HTTPHandler) getInstance(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	doc, err := inst.SettingsDocument()
	if err != nil {
		return err
	}

	doc.M["locale"] = inst.Locale
	doc.M["onboarding_finished"] = inst.OnboardingFinished
	if inst.PasswordDefined != nil {
		doc.M["password_defined"] = *inst.PasswordDefined
	}
	doc.M["auto_update"] = !inst.NoAutoUpdate
	doc.M["auth_mode"] = instance.AuthModeToString(inst.AuthMode)
	doc.M["tos"] = inst.TOSSigned
	doc.M["tos_latest"] = inst.TOSLatest
	doc.M["uuid"] = inst.UUID
	doc.M["oidc_id"] = inst.OIDCID
	doc.M["org_domain"] = inst.OrgDomain
	doc.M["org_id"] = inst.OrgID
	if oidcConfig, ok := config.GetOIDC(inst.ContextName); ok {
		if logoutURL, ok := oidcConfig["logout_url"].(string); ok && logoutURL != "" {
			doc.M["oidc_logout_url"] = logoutURL
		}
	}
	doc.M["context"] = inst.ContextName
	if len(inst.Sponsorships) > 0 {
		doc.M["sponsorships"] = inst.Sponsorships
	}
	// XXX we had a bug where the default_redirection was filled by a full URL
	// instead of slug+path, and we fix it when this endpoint is called.
	if value, ok := doc.M["default_redirection"].(string); !ok || strings.HasPrefix(value, "http") {
		doc.M["default_redirection"] = inst.DefaultAppAndPath()
	}

	// Allow any application with a token
	if _, err = middlewares.GetPermission(c); err != nil {
		return err
	}

	if url, err := h.svc.GetLegalNoticeUrl(inst); err != nil {
		inst.Logger().WithNamespace("settings").
			Warnf("Failed to fetch the legal notice URL from the cloudery: %s", err)
	} else if url != "" {
		doc.M["legal_notice_url"] = url
	}

	return jsonapi.Data(c, http.StatusOK, &apiInstance{doc}, nil)
}

func (h *HTTPHandler) updateInstance(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	doc := &couchdb.JSONDoc{}
	obj, err := jsonapi.Bind(c.Request().Body, doc)
	if err != nil {
		return err
	}

	if doc.M == nil {
		return jsonapi.BadJSON()
	}

	doc.Type = consts.Settings
	doc.SetID(consts.InstanceSettingsID)
	doc.SetRev(obj.Meta.Rev)

	if err = middlewares.Allow(c, permission.PUT, doc); err != nil {
		return err
	}

	pdoc, err := middlewares.GetPermission(c)
	if err != nil || pdoc.Type != permission.TypeCLI {
		delete(doc.M, "auth_mode")
		delete(doc.M, "tos")
		delete(doc.M, "tos_latest")
		delete(doc.M, "uuid")
		delete(doc.M, "context")
		delete(doc.M, "sponsorships")
		delete(doc.M, "oidc_id")

		// email and pending_email are never set through this generic update:
		// changing the email must go through the passphrase-protected
		// POST /settings/email flow (or the exempt CLI/admin path above), as it
		// is the delivery address for password-reset, 2FA and magic-link mails.
		// On signup-managed contexts, phone and recovery_email are additionally
		// owned by the external signup flow (their RabbitMQ event handlers), so
		// they are locked too. Restore the stored values so a client cannot set
		// them directly (e.g. an email to another user's address, bypassing
		// verification) nor erase them with a partial update. The settings doc is
		// replaced wholesale on patch, so the stored value is put back rather than
		// the incoming one merely dropped; a read failure aborts the request
		// instead of risking a wipe.
		guarded := []string{"email", "pending_email"}
		if inst.HasSignup() {
			guarded = append(guarded, "phone", "recovery_email")
		}
		current, err := inst.SettingsDocument()
		if err != nil {
			return err
		}
		for _, field := range guarded {
			if v, ok := current.M[field]; ok {
				doc.M[field] = v
			} else {
				delete(doc.M, field)
			}
		}
	}

	if err := lifecycle.Patch(inst, &lifecycle.Options{SettingsObj: doc}); err != nil {
		return err
	}

	doc.M["locale"] = inst.Locale
	doc.M["onboarding_finished"] = inst.OnboardingFinished
	doc.M["auto_update"] = !inst.NoAutoUpdate
	doc.M["auth_mode"] = instance.AuthModeToString(inst.AuthMode)
	doc.M["tos"] = inst.TOSSigned
	doc.M["tos_latest"] = inst.TOSLatest
	doc.M["uuid"] = inst.UUID
	doc.M["oidc_id"] = inst.OIDCID
	doc.M["org_domain"] = inst.OrgDomain
	doc.M["org_id"] = inst.OrgID
	if oidcConfig, ok := config.GetOIDC(inst.ContextName); ok {
		if logoutURL, ok := oidcConfig["logout_url"].(string); ok && logoutURL != "" {
			doc.M["oidc_logout_url"] = logoutURL
		}
	}
	doc.M["context"] = inst.ContextName

	return jsonapi.Data(c, http.StatusOK, &apiInstance{doc}, nil)
}

func (h *HTTPHandler) updateInstanceTOS(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	// Allow any request from OAuth tokens to use this route
	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	if pdoc.Type != permission.TypeOauth && pdoc.Type != permission.TypeCLI {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	if err := lifecycle.ManagerSignTOS(inst, c.Request()); err != nil {
		return err
	}

	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) updateInstanceAuthMode(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	if err := middlewares.AllowWholeType(c, permission.PUT, consts.Settings); err != nil {
		return err
	}

	args := struct {
		AuthMode                string `json:"auth_mode"`
		TwoFactorActivationCode string `json:"two_factor_activation_code"`
	}{}
	if err := c.Bind(&args); err != nil {
		return err
	}

	authMode, err := instance.StringToAuthMode(args.AuthMode)
	if err != nil {
		return jsonapi.BadRequest(err)
	}
	if inst.HasAuthMode(authMode) {
		return c.NoContent(http.StatusNoContent)
	}

	switch authMode {
	case instance.Basic:
	case instance.TwoFactorMail:
		if args.TwoFactorActivationCode == "" {
			if err = lifecycle.SendMailConfirmationCode(inst); err != nil {
				return err
			}
			return c.NoContent(http.StatusNoContent)
		}
		if ok := inst.ValidateMailConfirmationCode(args.TwoFactorActivationCode); !ok {
			return c.NoContent(http.StatusUnprocessableEntity)
		}
	}

	err = lifecycle.Patch(inst, &lifecycle.Options{AuthMode: args.AuthMode})
	if err != nil {
		return err
	}

	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) askInstanceDeletion(c echo.Context) error {
	if err := middlewares.RequireSettingsApp(c); err != nil {
		return err
	}

	inst := middlewares.GetInstance(c)
	if err := lifecycle.AskDeletion(inst); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) forceInstanceDeletion(c echo.Context) error {
	if err := middlewares.RequireSettingsApp(c); err != nil {
		return err
	}

	inst := middlewares.GetInstance(c)

	msg := rabbitmq.UserDeletionRequestedMessage{
		WorkplaceFqdn: inst.Domain,
		Reason:        "user_request",
		RequestedBy:   "cozy-stack",
		RequestedAt:   time.Now().UnixMilli(),
	}
	if err := h.rmq.Publish(c.Request().Context(), rabbitmq.PublishRequest{
		ContextName: inst.ContextName,
		Exchange:    rabbitmq.ExchangeAuth,
		RoutingKey:  rabbitmq.RoutingKeyUserDeletionRequested,
		Payload:     msg,
	}); err != nil {
		if errors.Is(err, rabbitmq.ErrNotConfigured) {
			explicitErr := errors.New("force instance deletion requires RabbitMQ to be configured")
			inst.Logger().Errorf("Force instance deletion publish failed: %s", err)
			return jsonapi.InternalServerError(explicitErr)
		}
		inst.Logger().Errorf("Force instance deletion publish failed: %s", err)
		return jsonapi.InternalServerError(err)
	}
	inst.Logger().Infof(
		"Force instance deletion requested: published %s to %s",
		rabbitmq.RoutingKeyUserDeletionRequested,
		rabbitmq.ExchangeAuth,
	)

	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) clearMovedFrom(c echo.Context) error {
	if !middlewares.IsLoggedIn(c) {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	inst := middlewares.GetInstance(c)
	doc, err := inst.SettingsDocument()
	if err != nil {
		return err
	}

	if doc.M["moved_from"] != nil {
		delete(doc.M, "moved_from")
		if err := couchdb.UpdateDoc(inst, doc); err != nil {
			return err
		}
	}

	return c.NoContent(http.StatusNoContent)
}
