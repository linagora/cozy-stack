package auth

import (
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/session"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// checkPasswordForShareByLink checks the password for a share by link
// protected by password, and creates a cookie if the password is correct.
func checkPasswordForShareByLink(c echo.Context) error {
	res := c.Response()
	origin := c.Request().Header.Get(echo.HeaderOrigin)
	res.Header().Set(echo.HeaderAccessControlAllowOrigin, origin)
	res.Header().Set(echo.HeaderAccessControlAllowCredentials, "true")
	res.Header().Add(echo.HeaderVary, echo.HeaderOrigin)

	inst := middlewares.GetInstance(c)
	permID := c.FormValue("perm_id")
	perm, err := permission.GetByID(inst, permID)
	if err != nil {
		if couchdb.IsNotFoundError(err) || errors.Is(err, permission.ErrExpiredToken) {
			return c.JSON(http.StatusNotFound, echo.Map{"error": err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}

	hash64, _ := perm.Password.(string)
	if len(hash64) == 0 {
		return c.JSON(http.StatusOK, echo.Map{"password": "none"})
	}
	hash, err := base64.StdEncoding.DecodeString(hash64)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}

	// Count per link, not per instance, and reset on success: the visitors of a
	// link share its counter, so only consecutive failures must add up.
	limiter := config.GetRateLimiter()
	limitKey := inst.DomainName() + ":" + permID
	if limits.IsLimitReachedOrExceeded(limiter.CheckRateLimitKey(limitKey, limits.SharePasswordType)) {
		return c.JSON(http.StatusTooManyRequests, echo.Map{"error": "Too many requests"})
	}

	password := []byte(c.FormValue("password"))
	_, err = crypto.CompareHashAndPassphrase(hash, password)
	if err != nil {
		msg := inst.Translate("Share by link Password Invalid")
		return c.JSON(http.StatusForbidden, echo.Map{"error": msg})
	}
	limiter.ResetCounterKey(limitKey, limits.SharePasswordType)

	// Put a cookie so that later requests can use the sharecode
	cookieName := "pass" + permID
	cfg := crypto.MACConfig{Name: cookieName, MaxLen: 256}
	encoded, err := crypto.EncodeAuthMessage(cfg, inst.SessionSecret(), []byte(permID), nil)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}
	cookie := &http.Cookie{
		Name:     cookieName,
		Value:    string(encoded),
		MaxAge:   0,
		Path:     "/",
		Domain:   session.CookieDomain(inst),
		Secure:   !build.IsDevRelease(),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	c.SetCookie(cookie)

	return c.JSON(http.StatusOK, echo.Map{"password": "ok"})
}
