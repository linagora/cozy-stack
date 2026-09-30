package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func registerClient(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	err := config.GetRateLimiter().CheckRateLimit(instance, limits.OAuthClientType)
	if limits.IsLimitReachedOrExceeded(err) {
		return echo.NewHTTPError(http.StatusNotFound, "Not found")
	}
	client := new(oauth.Client)
	if err := json.NewDecoder(c.Request().Body).Decode(client); err != nil {
		return err
	}
	// We do not allow the creation of clients allowed to have an empty scope
	// ("login" scope), except via the CLI.
	if client.AllowLoginScope {
		if _, ok := middlewares.GetCLIPermission(c); !ok {
			return echo.NewHTTPError(http.StatusUnauthorized,
				"Not authorized to create client with given parameters")
		}
	}
	if err := client.Create(instance); err != nil {
		return c.JSON(err.Code, err)
	}
	return c.JSON(http.StatusCreated, client)
}

func readClient(c echo.Context) error {
	client := c.Get("client").(*oauth.Client)
	client.TransformIDAndRev()
	return c.JSON(http.StatusOK, client)
}

func updateClient(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	err := config.GetRateLimiter().CheckRateLimit(instance, limits.OAuthClientType)
	if limits.IsLimitReachedOrExceeded(err) {
		return echo.NewHTTPError(http.StatusNotFound, "Not found")
	}

	clientID := c.Param("client-id")
	unlock, err := LockOAuthClient(instance, clientID)
	if err != nil {
		return err
	}
	defer unlock()

	oldClient, err := oauth.FindClient(instance, clientID)
	if err != nil {
		return c.JSON(http.StatusNotFound, echo.Map{
			"error": "Client not found",
		})
	}
	if err := checkClientToken(c, oldClient); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": err.Error(),
		})
	}

	client := new(oauth.Client)
	if err := json.NewDecoder(c.Request().Body).Decode(client); err != nil {
		return err
	}
	if err := client.Update(instance, oldClient); err != nil {
		return c.JSON(err.Code, err)
	}
	return c.JSON(http.StatusOK, client)
}

func deleteClient(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	clientID := c.Param("client-id")
	unlock, err := LockOAuthClient(instance, clientID)
	if err != nil {
		return err
	}
	defer unlock()

	client, err := oauth.FindClient(instance, clientID)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return c.NoContent(http.StatusNoContent)
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}
	if err := checkClientToken(c, client); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": err.Error(),
		})
	}

	// If the client has an OIDC session ID, perform SSO logout
	if client.OIDCSessionID != "" && instance.ContextName != "" {
		// Call the end_session_endpoint to terminate the SSO session
		// We don't fail the deletion if the logout fails, as this is best-effort
		_ = oauth.PerformOIDCLogout(instance.ContextName, client.OIDCSessionID)
	}

	if err := client.Delete(instance); err != nil {
		return c.JSON(err.Code, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func checkRegistrationToken(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		instance := middlewares.GetInstance(c)
		client, err := oauth.FindClient(instance, c.Param("client-id"))
		if err != nil {
			return c.JSON(http.StatusNotFound, echo.Map{
				"error": "Client not found",
			})
		}
		if err := checkClientToken(c, client); err != nil {
			return c.JSON(http.StatusUnauthorized, echo.Map{
				"error": err.Error(),
			})
		}
		c.Set("client", client)
		return next(c)
	}
}

func checkClientToken(c echo.Context, client *oauth.Client) error {
	header := c.Request().Header.Get(echo.HeaderAuthorization)
	if !strings.HasPrefix(header, "Bearer ") {
		return errors.New("invalid_token")
	}
	token := header[len("Bearer "):]
	instance := middlewares.GetInstance(c)
	_, ok := client.ValidToken(instance, consts.RegistrationTokenAudience, token)
	if !ok {
		return errors.New("invalid_token")
	}
	return nil
}
