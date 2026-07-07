package sharings

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func AddReadOnlyToGroup(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	_, err = checkCreatePermissions(c, s)
	if err != nil {
		if err = authorizeDelegatedGroupReadOnlyChange(c, s); err != nil {
			return err
		}
	}
	groupIndex, err := strconv.Atoi(c.Param("index"))
	if err != nil {
		return jsonapi.InvalidParameter("index", err)
	}
	if groupIndex < 0 || groupIndex >= len(s.Groups) {
		return jsonapi.InvalidParameter("index", errors.New("Invalid index"))
	}
	if s.Owner {
		if err = s.AddReadOnlyFlagToGroup(inst, groupIndex); err != nil {
			return wrapErrors(err)
		}
		go s.NotifyRecipients(inst, nil)
	} else {
		if err = s.DelegateAddReadOnlyFlagToGroup(inst, groupIndex); err != nil {
			return wrapErrors(err)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

func RemoveReadOnlyFromGroup(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sharingID := c.Param("sharing-id")
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return wrapErrors(err)
	}
	_, err = checkCreatePermissions(c, s)
	if err != nil {
		return err
	}
	groupIndex, err := strconv.Atoi(c.Param("index"))
	if err != nil {
		return jsonapi.InvalidParameter("index", err)
	}
	if groupIndex < 0 || groupIndex >= len(s.Groups) {
		return jsonapi.InvalidParameter("index", errors.New("Invalid index"))
	}
	if err = s.RemoveReadOnlyFlagFromGroup(inst, groupIndex); err != nil {
		return wrapErrors(err)
	}
	go s.NotifyRecipients(inst, nil)
	return c.NoContent(http.StatusNoContent)
}

func authorizeDelegatedGroupReadOnlyChange(c echo.Context, s *sharing.Sharing) error {
	if err := hasSharingWritePermissions(c); err != nil {
		return err
	}

	member, err := requestMember(c, s)
	if err != nil {
		return wrapErrors(err)
	}

	if member.ReadOnly {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	if s.OrgDrive {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	groupIndex, err := strconv.Atoi(c.Param("index"))
	if err != nil {
		return jsonapi.InvalidParameter("index", err)
	}
	if groupIndex < 0 || groupIndex >= len(s.Groups) {
		return jsonapi.InvalidParameter("index", errors.New("Invalid index"))
	}

	if s.Groups[groupIndex].Revoked {
		return echo.NewHTTPError(http.StatusBadRequest)
	}

	if c.Request().Method == http.MethodDelete {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	inGroup := false
	for _, idx := range member.Groups {
		if idx == groupIndex {
			inGroup = true
			break
		}
	}
	if !inGroup {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	if s.Groups[groupIndex].ReadOnly {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	return nil
}
