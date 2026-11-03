package move

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	jwt "github.com/golang-jwt/jwt/v5"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"
)

const (
	// MoveScope is the scope requested for a move (when we don't know yet if
	// the cozy will be the source or the target).
	MoveScope = consts.ExportsRequests + " " + consts.Imports
	// SourceClientID is the fake OAuth client ID used for some move endpoints.
	SourceClientID = "move"
)

// Request is a struct for confirming a move to another Cozy.
type Request struct {
	IgnoreVault bool               `json:"ignore_vault,omitempty"`
	SourceCreds RequestCredentials `json:"source_credentials"`
	TargetCreds RequestCredentials `json:"target_credentials"`
	Target      string             `json:"target"`
	Link        string             `json:"-"`
}

// RequestCredentials is struct for OAuth credentials (access_token, client_id
// and client_secret).
type RequestCredentials struct {
	Token        string `json:"token"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// TargetHost returns the host part of the target instance address.
func (r *Request) TargetHost() string {
	if u, err := url.Parse(r.Target); err == nil {
		return u.Host
	}
	return r.Target
}

// ImportingURL returns the URL on the target for the page to wait until
// the move is done.
func (r *Request) ImportingURL() string {
	u, err := url.Parse(r.Target)
	if err != nil {
		u, err = url.Parse("https://" + r.Target)
	}
	if err != nil {
		return r.Target
	}
	u.Path = "/move/importing"
	return u.String()
}

// CreateRequestClient creates an OAuth client that can be used for move requests.
func CreateRequestClient(inst *instance.Instance) (*oauth.Client, error) {
	client := &oauth.Client{
		RedirectURIs: []string{config.GetConfig().Move.URL + "/fake"},
		ClientName:   "cozy-stack",
		SoftwareID:   "github.com/cozy/cozy-stack",
	}
	if err := client.Create(inst, oauth.NotPending); err != nil {
		return nil, errors.New(err.Error)
	}
	return client, nil
}

// CreateRequest checks if the parameters are OK for moving, and if yes, it
// will persist them and return a link that can be used to confirm the move.
func CreateRequest(inst *instance.Instance, params url.Values) (*Request, error) {
	var source RequestCredentials
	code := params.Get("code")
	if code == "" {
		source.ClientID = params.Get("client_id")
		if source.ClientID == "" {
			return nil, errors.New("No client_id")
		}
		source.ClientSecret = params.Get("client_secret")
		if source.ClientSecret == "" {
			return nil, errors.New("No client_secret")
		}
		source.Token = params.Get("token")
		if source.Token == "" {
			return nil, errors.New("No code or token")
		}
		if err := checkSourceToken(inst, source); err != nil {
			return nil, err
		}
	} else {
		if err := checkSourceCode(inst, code); err != nil {
			return nil, err
		}
		client, err := CreateRequestClient(inst)
		if err != nil {
			return nil, err
		}
		client.CouchID = client.ClientID
		token, err := client.CreateJWT(inst, consts.AccessTokenAudience, MoveScope)
		if err != nil {
			return nil, err
		}
		source.ClientID = client.ClientID
		source.ClientSecret = client.ClientSecret
		source.Token = token
	}

	var target RequestCredentials
	cozyURL := params.Get("target_url")
	if cozyURL == "" {
		return nil, errors.New("No target_url")
	}
	if inst.HasDomain(cozyURL) {
		return nil, errors.New("Invalid target_url")
	}
	target.Token = params.Get("target_token")
	if target.Token == "" {
		return nil, errors.New("No target_token")
	}
	target.ClientID = params.Get("target_client_id")
	if target.ClientID == "" {
		return nil, errors.New("No target_client_id")
	}
	target.ClientSecret = params.Get("target_client_secret")
	if target.ClientSecret == "" {
		return nil, errors.New("No target_client_secret")
	}

	// If the user has clicked on the "Ignore this step" button in cozy-move at
	// the export the passwords page, we keep this information to not show them
	// how to import the passwords on the target instance.
	ignoreVault := params.Get("ignore_vault") != ""

	req := &Request{
		SourceCreds: source,
		TargetCreds: target,
		Target:      cozyURL,
		IgnoreVault: ignoreVault,
	}

	secret, err := GetStore().SaveRequest(inst, req)
	if err != nil {
		return nil, err
	}

	req.Link = inst.PageURL("/move/go", url.Values{"secret": {secret}})
	return req, nil
}

func checkSourceToken(inst *instance.Instance, source RequestCredentials) error {
	var claims permission.Claims
	err := crypto.ParseJWT(source.Token, func(token *jwt.Token) (interface{}, error) {
		return inst.PickKey(consts.AccessTokenAudience)
	}, &claims)
	if err != nil {
		return permission.ErrInvalidToken
	}

	if claims.Issuer != inst.Domain {
		return permission.ErrInvalidToken
	}
	if claims.Expired() {
		return permission.ErrExpiredToken
	}

	c, err := oauth.FindClient(inst, claims.Subject)
	if err != nil {
		if couchdb.IsInternalServerError(err) {
			return err
		}
		return permission.ErrInvalidToken
	}

	if c.ClientID != source.ClientID {
		return permission.ErrInvalidToken
	}
	if c.ClientSecret != source.ClientSecret {
		return permission.ErrInvalidToken
	}
	return nil
}

func checkSourceCode(inst *instance.Instance, code string) error {
	accessCode := &oauth.AccessCode{}
	if err := couchdb.GetDoc(inst, consts.OAuthAccessCodes, code, accessCode); err != nil {
		return permission.ErrInvalidToken
	}
	if accessCode.ClientID != SourceClientID {
		return permission.ErrInvalidToken
	}
	if accessCode.Scope != consts.ExportsRequests {
		return permission.ErrInvalidToken
	}
	return nil
}

// StartMove checks that the secret is known, sends a request to the other Cozy
// to block it during the move, and pushs a job for the export.
func StartMove(inst *instance.Instance, secret string) (*Request, error) {
	req, err := GetStore().GetRequest(inst, secret)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("Invalid secret")
	}

	u := req.ImportingURL() + "?source=" + inst.ContextualDomain()
	r, err := http.NewRequest("POST", u, nil)
	if err != nil {
		return nil, errors.New("Cannot reach the other Cozy")
	}
	r.Header.Add(echo.HeaderAuthorization, "Bearer "+req.TargetCreds.Token)
	_, err = safehttp.ClientWithKeepAlive.Do(r)
	if err != nil {
		return nil, errors.New("Cannot reach the other Cozy")
	}

	doc, err := inst.SettingsDocument()
	if err == nil {
		doc.M["moved_to"] = req.Target
		_ = couchdb.UpdateDoc(inst, doc)
	}

	options := ExportOptions{
		ContextualDomain: inst.ContextualDomain(),
		TokenSource:      req.SourceCreds.Token,
		MoveTo: &MoveToOptions{
			URL:          req.Target,
			Token:        req.TargetCreds.Token,
			ClientID:     req.TargetCreds.ClientID,
			ClientSecret: req.TargetCreds.ClientSecret,
		},
		IgnoreVault: req.IgnoreVault,
	}
	msg, err := job.NewMessage(options)
	if err != nil {
		return nil, err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "export",
		Message:    msg,
	})
	return req, err
}

// CallFinalize will call the /move/finalize endpoint on the other instance to
// unblock it after a successful move.
func CallFinalize(inst *instance.Instance, otherURL, token string, vault bool) {
	u, err := url.Parse(otherURL)
	if err != nil {
		u, err = url.Parse("https://" + otherURL)
	}
	if err != nil {
		return
	}
	u.Path = "/move/finalize"
	subdomainType := "flat"
	if config.GetConfig().Subdomains == config.NestedSubdomains {
		subdomainType = "nested"
	}
	u.RawQuery = url.Values{"subdomain": {subdomainType}}.Encode()
	req, err := http.NewRequest("POST", u.String(), nil)
	if err != nil {
		inst.Logger().
			WithNamespace("move").
			WithField("url", otherURL).
			Warnf("Cannot finalize: %s", err)
		return
	}
	req.Header.Add(echo.HeaderAuthorization, "Bearer "+token)
	res, err := safehttp.ClientWithKeepAlive.Do(req)
	if err != nil {
		inst.Logger().
			WithNamespace("move").
			WithField("url", otherURL).
			Warnf("Cannot finalize: %s", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 204 {
		inst.Logger().
			WithNamespace("move").
			WithField("url", otherURL).
			Warnf("Cannot finalize: code=%d", res.StatusCode)
	}

	doc, err := inst.SettingsDocument()
	if err == nil {
		doc.M["moved_from"] = u.Host
		if vault {
			doc.M["import_vault"] = true
		}
		if err := couchdb.UpdateDoc(inst, doc); err != nil {
			inst.Logger().
				WithNamespace("move").
				WithField("moved_from", u.Host).
				WithField("vault", strconv.FormatBool(vault)).
				Warnf("Cannot save settings: %s", err)
		}
	}
}

// Finalize makes the last steps on the source Cozy after the data has been
// successfully imported:
// - stop the konnectors
// - warn the OAuth clients
// - unblock the instance
// - ask the manager to delete the instance in one month
// - tell the members of the sharings to follow the move in their recents
func Finalize(inst *instance.Instance, subdomainType string) error {
	var errm error
	sched := job.System()
	triggers, err := sched.GetAllTriggers(inst)
	if err == nil {
		for _, t := range triggers {
			infos := t.Infos()
			if infos.WorkerType == "konnector" {
				if err = sched.DeleteTrigger(inst, infos.TID); err != nil {
					errm = multierror.Append(errm, err)
				}
			}
		}
	} else {
		errm = multierror.Append(errm, err)
	}
	inst.Moved = true
	if err := lifecycle.Unblock(inst); err != nil {
		errm = multierror.Append(errm, err)
	}

	doc, err := inst.SettingsDocument()
	if err == nil {
		doc.M["moved_to_subdomain_type"] = subdomainType
		err = couchdb.UpdateDoc(inst, doc)
	}
	if err != nil {
		errm = multierror.Append(errm, err)
	}

	if err := askManagerToDeleteInstance(inst); err != nil {
		errm = multierror.Append(errm, err)
	}

	// Last, as it reads all the sharings: the reads of the recents also
	// migrate the rows on the items of a moved instance, so a failure here
	// does not fail the move.
	if err := recents.FollowMove(inst); err != nil {
		inst.Logger().WithNamespace("move").
			Warnf("Cannot follow the move in the recents: %s", err)
	}

	return errm
}

// DelayBeforeInstanceDeletionAfterMoved is the one month delay before an
// instance is deleted after it has been moved to a new address.
const DelayBeforeInstanceDeletionAfterMoved = 30 * 24 * time.Hour

func askManagerToDeleteInstance(inst *instance.Instance) error {
	if inst.UUID == "" {
		return nil
	}

	client := instance.APIManagerClient(inst)
	if client == nil {
		return nil
	}

	ts := time.Now().Add(DelayBeforeInstanceDeletionAfterMoved)
	url := fmt.Sprintf("/api/v1/instances/%s?date=%d", url.PathEscape(inst.UUID), ts.Unix())
	return client.Delete(url)
}

// Abort will call the /move/abort endpoint on the other instance to unblock it
// after a failed export or import during a move.
func Abort(inst *instance.Instance, otherURL, token string) {
	u, err := url.Parse(otherURL)
	if err != nil {
		u, err = url.Parse("https://" + otherURL)
	}
	if err != nil {
		return
	}
	u.Path = "/move/abort"
	req, err := http.NewRequest("POST", u.String(), nil)
	if err != nil {
		inst.Logger().
			WithNamespace("move").
			WithField("url", otherURL).
			Warnf("Cannot abort: %s", err)
		return
	}
	req.Header.Add(echo.HeaderAuthorization, "Bearer "+token)
	res, err := safehttp.ClientWithKeepAlive.Do(req)
	if err != nil {
		inst.Logger().
			WithNamespace("move").
			WithField("url", otherURL).
			Warnf("Cannot abort: %s", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 204 {
		inst.Logger().
			WithNamespace("move").
			WithField("url", otherURL).
			Warnf("Cannot abort: code=%d", res.StatusCode)
	}
}
