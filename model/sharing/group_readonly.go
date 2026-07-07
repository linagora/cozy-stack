package sharing

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"
)

func (s *Sharing) CheckMemberGroupReadOnlyConsistency(memberIndex int) error {
	for _, gidx := range s.Members[memberIndex].Groups {
		if gidx < 0 || gidx >= len(s.Groups) {
			continue
		}
		if s.Groups[gidx].Revoked {
			continue
		}
		return ErrGroupReadOnlyConflict
	}
	return nil
}

func (s *Sharing) checkGroupReadOnlyChangeConsistency(groupIndex int, targetReadOnly bool) error {
	for _, m := range s.Members {
		inGroup := false
		for _, idx := range m.Groups {
			if idx == groupIndex {
				inGroup = true
				break
			}
		}
		if !inGroup {
			continue
		}
		for _, otherIdx := range m.Groups {
			if otherIdx == groupIndex {
				continue
			}
			if otherIdx < 0 || otherIdx >= len(s.Groups) {
				continue
			}
			if s.Groups[otherIdx].Revoked {
				continue
			}
			if s.Groups[otherIdx].ReadOnly != targetReadOnly {
				return ErrGroupReadOnlyConflict
			}
		}
	}
	return nil
}

func (s *Sharing) checkGroupMembersIndividualConsistency(groupIndex int, targetReadOnly bool) error {
	for _, m := range s.Members {
		inGroup := false
		for _, idx := range m.Groups {
			if idx == groupIndex {
				inGroup = true
				break
			}
		}
		if !inGroup {
			continue
		}
		if !m.OnlyInGroups && m.ReadOnly != targetReadOnly {
			return ErrGroupReadOnlyConflict
		}
	}
	return nil
}

func (s *Sharing) AddReadOnlyFlagToGroup(inst *instance.Instance, groupIndex int) error {
	if !s.Owner {
		return ErrInvalidSharing
	}
	if groupIndex < 0 || groupIndex >= len(s.Groups) {
		return ErrInvalidSharing
	}
	if s.Groups[groupIndex].Revoked {
		return ErrInvalidSharing
	}
	if s.Groups[groupIndex].ReadOnly {
		return nil
	}
	if err := s.checkGroupReadOnlyChangeConsistency(groupIndex, true); err != nil {
		return err
	}
	if err := s.checkGroupMembersIndividualConsistency(groupIndex, true); err != nil {
		return err
	}
	var errm error
	for i, m := range s.Members {
		if i == 0 {
			continue
		}
		inGroup := false
		for _, idx := range m.Groups {
			if idx == groupIndex {
				inGroup = true
				break
			}
		}
		if !inGroup {
			continue
		}
		if s.Members[i].ReadOnly {
			continue
		}
		if err := s.AddReadOnlyFlag(inst, i); err != nil {
			errm = multierror.Append(errm, err)
		}
	}
	if errm == nil {
		s.Groups[groupIndex].ReadOnly = true
		if err := couchdb.UpdateDoc(inst, s); err != nil {
			errm = err
		}
	}
	return errm
}

func (s *Sharing) RemoveReadOnlyFlagFromGroup(inst *instance.Instance, groupIndex int) error {
	if !s.Owner {
		return ErrInvalidSharing
	}
	if groupIndex < 0 || groupIndex >= len(s.Groups) {
		return ErrInvalidSharing
	}
	if s.Groups[groupIndex].Revoked {
		return ErrInvalidSharing
	}
	if !s.Groups[groupIndex].ReadOnly {
		return nil
	}
	if err := s.checkGroupReadOnlyChangeConsistency(groupIndex, false); err != nil {
		return err
	}
	if err := s.checkGroupMembersIndividualConsistency(groupIndex, false); err != nil {
		return err
	}
	var errm error
	for i, m := range s.Members {
		if i == 0 {
			continue
		}
		inGroup := false
		for _, idx := range m.Groups {
			if idx == groupIndex {
				inGroup = true
				break
			}
		}
		if !inGroup {
			continue
		}
		if !s.Members[i].ReadOnly {
			continue
		}
		if err := s.RemoveReadOnlyFlag(inst, i); err != nil {
			errm = multierror.Append(errm, err)
		}
	}
	if errm == nil {
		s.Groups[groupIndex].ReadOnly = false
		if err := couchdb.UpdateDoc(inst, s); err != nil {
			errm = err
		}
	}
	return errm
}

func (s *Sharing) DelegateAddReadOnlyFlagToGroup(inst *instance.Instance, groupIndex int) error {
	if len(s.Credentials) != 1 {
		return ErrInvalidSharing
	}
	m := &s.Members[0]
	u, ok := m.InstanceURL()
	if !ok {
		return ErrInvalidSharing
	}
	c := &s.Credentials[0]
	if c.AccessToken == nil {
		return ErrInvalidSharing
	}
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   fmt.Sprintf("/sharings/%s/groups/%d/readonly", s.SID, groupIndex),
		Headers: request.Headers{
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, m, c, opts, nil)
	}
	if err != nil {
		if res != nil && res.StatusCode == http.StatusBadRequest {
			if reqErr, ok := err.(*request.Error); ok && strings.Contains(reqErr.Detail, ErrGroupReadOnlyConflict.Error()) {
				return ErrGroupReadOnlyConflict
			}
			return ErrInvalidURL
		}
		return err
	}
	res.Body.Close()
	return nil
}
