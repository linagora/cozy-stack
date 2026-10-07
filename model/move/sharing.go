package move

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"
)

// NotifySharings will notify other instances with a common sharing that this
// instance has moved, and will tell them to the new URL to use for the
// sharing.
func NotifySharings(inst *instance.Instance) error {
	// Let the dust settle a bit before starting the notifications
	time.Sleep(3 * time.Second)

	var errm error

	err := couchdb.ForeachDocs(inst, consts.Sharings, func(id string, doc json.RawMessage) error {
		var s sharing.Sharing
		if err := json.Unmarshal(doc, &s); err != nil {
			return err
		}

		time.Sleep(100 * time.Millisecond)
		if err := notifySharing(inst, &s); err != nil {
			errm = multierror.Append(errm, err)
		}
		return nil
	})

	if err != nil {
		errm = multierror.Append(errm, err)
	}

	return errm
}

func notifySharing(inst *instance.Instance, s *sharing.Sharing) error {
	if !s.Owner {
		return notifyMember(inst, s, 0)
	}

	var errm error
	for i := range s.Members {
		if i == 0 {
			continue // skip the owner
		}
		if err := notifyMember(inst, s, i); err != nil {
			errm = multierror.Append(errm, err)
		}
	}
	return errm
}

func notifyMember(inst *instance.Instance, s *sharing.Sharing, index int) error {
	u, err := url.Parse(s.Members[index].Instance)
	if s.Members[index].Instance == "" || err != nil {
		return err
	}

	if len(s.Credentials) == 0 {
		return errors.New("sharing in invalid state")
	}
	clientID := s.Credentials[0].InboundClientID
	if index > 0 {
		clientID = s.Credentials[index-1].InboundClientID
	}
	cli := &oauth.Client{ClientID: clientID}
	newToken, err := sharing.CreateAccessToken(inst, cli, s.ID(), permission.ALL)
	if err != nil {
		return err
	}
	moved := sharing.APIMoved{
		SharingID:    s.ID(),
		NewInstance:  inst.PageURL("", nil),
		AccessToken:  newToken.AccessToken,
		RefreshToken: newToken.RefreshToken,
	}
	data, err := jsonapi.MarshalObject(&moved)
	if err != nil {
		return err
	}
	body, err := json.Marshal(jsonapi.Document{Data: &data})
	if err != nil {
		return err
	}

	credIndex := 0
	if s.Owner {
		credIndex = index - 1
	}
	if len(s.Credentials) <= credIndex || s.Credentials[credIndex].AccessToken == nil {
		return errors.New("sharing in invalid state")
	}
	token := s.Credentials[credIndex].AccessToken.AccessToken

	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/recipients/self/moved",
		Headers: request.Headers{
			echo.HeaderAccept:        jsonapi.ContentType,
			echo.HeaderContentType:   jsonapi.ContentType,
			echo.HeaderAuthorization: "Bearer " + token,
		},
		Body:       bytes.NewReader(body),
		ParseError: sharing.ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && sharing.ShouldRefreshToken(res.StatusCode) {
		res, err = sharing.RefreshToken(inst, res, err, s, &s.Members[index], &s.Credentials[credIndex], opts, body)
	}
	if err != nil {
		return err
	}
	defer res.Body.Close()

	return nil
}

// UpdateSelfMemberInstance updates the instance URL for the current instance
// in all local sharing documents. This is needed after a domain migration
// because the sharing documents still reference the old domain for "self".
func UpdateSelfMemberInstance(inst *instance.Instance) error {
	if inst.OldDomain == "" {
		inst.Logger().WithNamespace("move").
			Infof("No OldDomain set on instance, skipping sharing member update")
		return nil
	}

	newInstance := inst.PageURL("", nil)
	var errm error

	err := couchdb.ForeachDocs(inst, consts.Sharings, func(id string, doc json.RawMessage) error {
		var s sharing.Sharing
		if err := json.Unmarshal(doc, &s); err != nil {
			return err
		}

		updated := false

		if s.Owner {
			// We are the owner, update members[0].Instance
			if len(s.Members) > 0 && s.Members[0].Instance != newInstance {
				inst.Logger().WithNamespace("move").
					Infof("Updating owner instance in sharing %s from %s to %s",
						s.SID, s.Members[0].Instance, newInstance)
				s.Members[0].Instance = newInstance
				updated = true
			}
		} else {
			// We are a recipient, find our member entry by matching OldDomain
			for i := range s.Members {
				if i == 0 {
					continue // skip the owner
				}
				if s.Members[i].Instance == newInstance {
					continue // already has the correct instance
				}
				if s.Members[i].Status == sharing.MemberStatusRevoked {
					continue
				}

				// Check if this member's instance matches our old domain exactly
				memberURL, err := url.Parse(s.Members[i].Instance)
				if err != nil {
					inst.Logger().WithNamespace("move").
						Warnf("Error parsing during sharings updata %s", s.Members[i].Instance)
					continue
				}
				if memberURL.Host == inst.OldDomain {
					inst.Logger().WithNamespace("move").
						Infof("Found self in sharing %s: updating member %d from %s to %s",
							s.SID, i, s.Members[i].Instance, newInstance)
					s.Members[i].Instance = newInstance
					updated = true
					break
				}
			}
		}

		if updated {
			if err := couchdb.UpdateDoc(inst, &s); err != nil {
				errm = multierror.Append(errm, err)
			}
		}

		return nil
	})

	if err != nil {
		errm = multierror.Append(errm, err)
	}

	return errm
}

// UpdateTriggersAfterMove updates the domain field in all trigger documents
// to use the current instance domain. This is needed after a domain migration
// because triggers store the domain at creation time.
func UpdateTriggersAfterMove(inst *instance.Instance) error {
	sched := job.System()
	triggers, err := sched.GetAllTriggers(inst)
	if err != nil {
		return err
	}

	newDomain := inst.Domain
	var errm error

	for _, t := range triggers {
		infos := t.Infos()
		if infos.Domain == newDomain {
			continue // Already has the correct domain
		}

		inst.Logger().WithNamespace("move").
			Infof("Updating trigger %s domain from %s to %s", infos.TID, infos.Domain, newDomain)

		// Update the domain in the trigger infos
		infos.Domain = newDomain

		// Save the updated trigger to CouchDB
		if err := couchdb.UpdateDoc(inst, infos); err != nil {
			errm = multierror.Append(errm, err)
		}
	}

	// Rebuild Redis to pick up the changes
	if err := sched.RebuildRedis(inst); err != nil {
		errm = multierror.Append(errm, err)
	}

	return errm
}
