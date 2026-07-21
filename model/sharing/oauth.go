package sharing

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

const InteractiveSharingQueryParam = "interactive"

// CreateSharingRequest sends information about the sharing to the recipient's cozy
func (m *Member) CreateSharingRequest(inst *instance.Instance, s *Sharing, c *Credentials, u *url.URL) error {
	if len(c.XorKey) == 0 {
		return ErrInvalidSharing
	}

	rules := s.Rules
	if !s.Drive {
		rules = make([]Rule, 0, len(s.Rules))
		for _, rule := range s.Rules {
			if rule.Local {
				continue
			}
			if rule.FilesByID() && len(rule.Values) > 0 {
				if fileDoc, err := inst.VFS().FileByID(rule.Values[0]); err == nil {
					// err != nil means that the target is a directory and not
					// a file, and we leave the mime blank in that case.
					rule.Mime = fileDoc.Mime
				}
			}
			values := make([]string, len(rule.Values))
			for i, v := range rule.Values {
				switch rule.Selector {
				case "", "id", "_id", "organization_id":
					values[i] = XorID(v, c.XorKey)
				case couchdb.SelectorReferencedBy:
					parts := strings.SplitN(v, "/", 2)
					values[i] = parts[0] + "/" + XorID(parts[1], c.XorKey)
				default:
					values[i] = v
				}
			}
			rule.Values = values
			rules = append(rules, rule)
		}
	}

	var memberIndex int
	members := make([]Member, len(s.Members))
	for i, m := range s.Members {
		// Instance and name are private...
		members[i] = Member{
			Status:       m.Status,
			PublicName:   m.PublicName,
			Email:        m.Email,
			ReadOnly:     m.ReadOnly,
			OnlyInGroups: m.OnlyInGroups,
			Groups:       m.Groups,
		}
		// ... except for the sharer and the recipient of this request
		if i == 0 || &s.Credentials[i-1] == c {
			memberIndex = i
			members[i].Instance = m.Instance
		}
	}

	var creds *interface{}
	if s.Drive {
		token, err := s.GetInteractCode(inst, &s.Members[memberIndex], memberIndex)
		if err != nil {
			return err
		}
		var list interface{} = []Credentials{{
			DriveToken: token,
			State:      c.State,
		}}
		creds = &list
	}

	sh := APISharing{
		&Sharing{
			SID:           s.SID,
			Drive:         s.Drive,
			DriveRootType: s.DriveRootType,
			OrgDrive:      s.OrgDrive,
			Active:        false,
			Owner:         false,
			Open:          s.Open,
			AccessMode:    s.AccessMode,
			Description:   s.Description,
			AppSlug:       s.AppSlug,
			PreviewPath:   s.PreviewPath,
			CreatedAt:     s.CreatedAt,
			UpdatedAt:     s.UpdatedAt,
			Rules:         rules,
			Members:       members,
			Groups:        s.Groups,
			NbFiles:       s.countFiles(inst),
		},
		creds,
		nil,
	}
	data, err := jsonapi.MarshalObject(&sh)
	if err != nil {
		return err
	}
	body, err := json.Marshal(jsonapi.Document{Data: &data})
	if err != nil {
		return err
	}
	opts := request.Options{
		Method: http.MethodPut,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID,
		Headers: request.Headers{
			echo.HeaderAccept:      jsonapi.ContentType,
			echo.HeaderContentType: jsonapi.ContentType,
		},
		Queries: u.Query(),
		Body:    bytes.NewReader(body),
	}
	res, err := request.Req(&opts)
	if res != nil && res.StatusCode == http.StatusConflict {
		return ErrAlreadyAccepted
	}
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// countFiles returns the number of files that should be uploaded on the
// initial synchronisation.
func (s *Sharing) countFiles(inst *instance.Instance) int {
	count := 0
	for _, rule := range s.Rules {
		if rule.DocType != consts.Files || rule.Local || len(rule.Values) == 0 {
			continue
		}

		if rule.Selector == "" || rule.Selector == "id" {
			fs := inst.VFS()
			for _, fileID := range rule.Values {
				dir, _, err := fs.DirOrFileByID(fileID)
				if err != nil {
					continue
				}
				if dir != nil {
					nb, err := countFilesInDirectory(inst, dir)
					if err != nil {
						continue
					}
					count += nb
				} else {
					count++
				}
			}
		} else {
			var resCount couchdb.ViewResponse
			for _, val := range rule.Values {
				reqCount := &couchdb.ViewRequest{Key: val, Reduce: true}
				err := couchdb.ExecView(inst, couchdb.FilesReferencedByView, reqCount, &resCount)
				if err == nil && len(resCount.Rows) > 0 {
					count += int(resCount.Rows[0].Value.(float64))
				}
			}
		}
	}
	return count
}

func countFilesInDirectory(inst *instance.Instance, dir *vfs.DirDoc) (int, error) {
	// Find the subdirectories
	start := dir.Fullpath + "/"
	stop := dir.Fullpath + "0" // 0 is the next ascii character after /
	if dir.DocID == consts.RootDirID {
		start = "/"
		stop = "0"
	}
	sel := mango.And(
		mango.Gt("path", start),
		mango.Lt("path", stop),
		mango.Equal("type", consts.DirType),
	)
	req := &couchdb.FindRequest{
		UseIndex: "dir-by-path",
		Selector: sel,
		Fields:   []string{"_id"},
		Limit:    10000,
	}
	var children []couchdb.JSONDoc
	err := couchdb.FindDocs(inst, consts.Files, req, &children)
	if err != nil {
		return 0, err
	}
	keys := make([]interface{}, len(children)+1)
	keys[0] = dir.DocID
	for i, child := range children {
		keys[i+1] = child.ID()
	}

	// Get the number of files for the directory and each of its sub-directory
	var resp couchdb.ViewResponse
	err = couchdb.ExecView(inst, couchdb.DiskUsageView, &couchdb.ViewRequest{
		Keys:  keys,
		Limit: 100_000,
	}, &resp)
	if err != nil {
		return 0, err
	}
	return len(resp.Rows), nil
}

// RegisterCozyURL saves a new Cozy URL for a member
func (s *Sharing) RegisterCozyURL(inst *instance.Instance, m *Member, cozyURL string) error {
	return s.registerCozyURL(inst, m, cozyURL, false)
}

// RegisterCozyURLInteractive saves a Cozy URL discovered during an interactive
// acceptance flow and marks the recipient request so auto-accept is skipped.
func (s *Sharing) RegisterCozyURLInteractive(inst *instance.Instance, m *Member, cozyURL string) error {
	return s.registerCozyURL(inst, m, cozyURL, true)
}

func (s *Sharing) registerCozyURL(inst *instance.Instance, m *Member, cozyURL string, interactive bool) error {
	if !s.Owner {
		return ErrInvalidSharing
	}
	if m.Status == MemberStatusReady {
		return ErrAlreadyAccepted
	}
	if m.Status == MemberStatusOwner || m.Status == MemberStatusRevoked {
		return ErrMemberNotFound
	}

	cozyURL = strings.TrimSpace(cozyURL)
	if !strings.Contains(cozyURL, "://") {
		cozyURL = "https://" + cozyURL
	}
	u, err := url.Parse(cozyURL)
	if err != nil || u.Host == "" {
		return ErrInvalidURL
	}
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	m.Instance = u.String()
	if interactive {
		q := u.Query()
		q.Set(InteractiveSharingQueryParam, "true")
		u.RawQuery = q.Encode()
	}

	creds := s.FindCredentials(m)
	if creds == nil {
		return ErrInvalidSharing
	}
	if err = m.CreateSharingRequest(inst, s, creds, u); err != nil {
		inst.Logger().WithNamespace("sharing").Warnf("Error on sharing request: %s", err)
		if errors.Is(err, ErrAlreadyAccepted) {
			return err
		}
		return ErrRequestFailed
	}
	return couchdb.UpdateDoc(inst, s)
}

// GenerateOAuthURL takes care of creating a correct OAuth request for
// the given member of the sharing.
func (m *Member) GenerateOAuthURL(s *Sharing, shortcut string) (string, error) {
	if !s.Owner || len(s.Members) != len(s.Credentials)+1 {
		return "", ErrInvalidSharing
	}
	creds := s.FindCredentials(m)
	if creds == nil {
		return "", ErrInvalidSharing
	}
	if m.Instance == "" {
		return "", ErrNoOAuthClient
	}

	u, err := url.Parse(m.Instance)
	if err != nil {
		return "", err
	}
	u.Path = "/auth/authorize/sharing"

	q := url.Values{
		"sharing_id": {s.SID},
		"state":      {creds.State},
	}
	if shortcut != "" {
		q.Add("shortcut", shortcut)
	}
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// CreateOAuthClient creates an OAuth client for a recipient of the given sharing
func CreateOAuthClient(inst *instance.Instance, m *Member) (*oauth.Client, error) {
	if m.Instance == "" {
		return nil, ErrInvalidURL
	}
	cli := oauth.Client{
		RedirectURIs: []string{m.Instance + "/sharings/answer"},
		ClientName:   "Sharing " + m.PublicName,
		ClientKind:   "sharing",
		SoftwareID:   "github.com/cozy/cozy-stack",
		ClientURI:    m.Instance + "/",
	}
	if err := cli.Create(inst, oauth.NotPending); err != nil {
		return nil, ErrInternalServerError
	}
	return &cli, nil
}

// DeleteOAuthClient removes the client associated to the given member
func DeleteOAuthClient(inst *instance.Instance, m *Member, cred *Credentials) error {
	if m.Instance == "" {
		return ErrInvalidURL
	}
	clientID := cred.InboundClientID
	if clientID == "" {
		return nil
	}
	client, err := oauth.FindClient(inst, clientID)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil
		}
		return err
	}
	if cerr := client.Delete(inst); cerr != nil {
		return errors.New(cerr.Error)
	}
	return nil
}

// ConvertOAuthClient converts an OAuth client from one type
// (model/oauth.Client) to another (client/auth.Client)
func ConvertOAuthClient(c *oauth.Client) *auth.Client {
	return &auth.Client{
		ClientID:          c.ClientID,
		ClientSecret:      c.ClientSecret,
		SecretExpiresAt:   c.SecretExpiresAt,
		RegistrationToken: c.RegistrationToken,
		RedirectURIs:      c.RedirectURIs,
		ClientName:        c.ClientName,
		ClientKind:        c.ClientKind,
		ClientURI:         c.ClientURI,
		LogoURI:           c.LogoURI,
		PolicyURI:         c.PolicyURI,
		SoftwareID:        c.SoftwareID,
		SoftwareVersion:   c.SoftwareVersion,
	}
}

// CreateAccessToken creates an access token for the given OAuth client,
// with a scope on this sharing.
func CreateAccessToken(inst *instance.Instance, cli *oauth.Client, sharingID string, verb permission.VerbSet) (*auth.AccessToken, error) {
	scope := consts.Sharings + ":" + verb.String() + ":" + sharingID
	cli.CouchID = cli.ClientID // XXX CouchID is required by CreateJWT
	refresh, err := cli.CreateJWT(inst, consts.RefreshTokenAudience, scope)
	if err != nil {
		return nil, err
	}
	access, err := cli.CreateJWT(inst, consts.AccessTokenAudience, scope)
	if err != nil {
		return nil, err
	}
	return &auth.AccessToken{
		TokenType:    "bearer",
		AccessToken:  access,
		RefreshToken: refresh,
		Scope:        scope,
	}, nil
}

// updateSharingWithConflictRetry persists the sharing and resolves stale-revision
// conflicts by reconciling the desired changes onto the latest revision.
// reconcile can mutate latest for the next retry and should return true when the
// latest revision already contains a usable final state and no write is needed.
func updateSharingWithConflictRetry(
	inst *instance.Instance,
	s *Sharing,
	retries int,
	reconcile func(latest *Sharing) bool,
) error {
	log := inst.Logger().WithNamespace("sharing")
	current := s
	for attempt := 0; ; attempt++ {
		err := couchdb.UpdateDoc(inst, current)
		if err == nil {
			*s = *current
			return nil
		}
		if !couchdb.IsConflictError(err) {
			return err
		}

		latest, findErr := FindSharing(inst, s.SID)
		if findErr != nil {
			return err
		}
		if reconcile(latest) {
			*s = *latest
			return nil
		}
		if attempt >= retries {
			return err
		}
		log.Debugf("Conflict while updating sharing %s, retrying with latest revision (%d/%d)", s.SID, attempt+1, retries)
		current = latest
	}
}

// SendAnswer says to the sharer's Cozy that the sharing has been accepted, and
// materialize that by an exchange of credentials.
func (s *Sharing) SendAnswer(inst *instance.Instance, state string) error {
	if s.Owner || len(s.Members) < 2 || len(s.Credentials) != 1 {
		return ErrInvalidSharing
	}
	u, err := url.Parse(s.Members[0].Instance)
	if s.Members[0].Instance == "" || err != nil {
		return ErrInvalidSharing
	}
	cli, err := CreateOAuthClient(inst, &s.Members[0])
	if err != nil {
		return err
	}
	token, err := CreateAccessToken(inst, cli, s.SID, permission.ALL)
	if err != nil {
		return err
	}
	name, err := csettings.PublicName(inst)
	if err != nil {
		inst.Logger().WithNamespace("sharing").
			Infof("No name for instance %v", inst)
	}
	ac := APICredentials{
		Credentials: &Credentials{
			State:       state,
			Client:      ConvertOAuthClient(cli),
			AccessToken: token,
		},
		PublicName: name,
		CID:        s.SID,
	}
	if s.FirstBitwardenOrganizationRule() != nil {
		setting, err := settings.Get(inst)
		if err != nil {
			return err
		}
		ac.Bitwarden = &APIBitwarden{
			UserID:    inst.ID(),
			PublicKey: setting.PublicKey,
		}
	}
	data, err := jsonapi.MarshalObject(&ac)
	if err != nil {
		return err
	}
	body, err := json.Marshal(jsonapi.Document{Data: &data})
	if err != nil {
		return err
	}
	res, err := request.Req(&request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/answer",
		Headers: request.Headers{
			echo.HeaderAccept:      jsonapi.ContentType,
			echo.HeaderContentType: jsonapi.ContentType,
		},
		Body: bytes.NewReader(body),
	})
	if err != nil {
		return err
	}
	defer res.Body.Close()

	for i, m := range s.Members {
		if i > 0 && m.Instance != "" {
			if m.Status == MemberStatusMailNotSent ||
				m.Status == MemberStatusPendingInvitation ||
				m.Status == MemberStatusSeen {
				s.Members[i].Status = MemberStatusReady
			}
		}
	}

	if err = s.SetupReceiver(inst); err != nil {
		return err
	}

	var creds Credentials
	if _, err = jsonapi.Bind(res.Body, &creds); err != nil {
		return ErrRequestFailed
	}
	s.Credentials[0].XorKey = creds.XorKey
	s.Credentials[0].InboundClientID = cli.ClientID
	s.Credentials[0].AccessToken = creds.AccessToken
	s.Credentials[0].Client = creds.Client
	s.Active = true
	s.Initial = s.NbFiles > 0

	options := config.GetSharingConfig(inst.ContextName)
	// Mark the sender's contact as trusted since we accepted their sharing
	if options.AutoAcceptTrustedContacts && len(s.Members) > 0 && s.Members[0].Email != "" {
		c, err := contact.FindByEmail(inst, s.Members[0].Email)
		if err != nil {
			// Contact doesn't exist, create it using the standardized method
			c, err = contact.Create(inst, contact.CreateOptions{
				Email:   s.Members[0].Email,
				Name:    s.Members[0].Name,
				CozyURL: s.Members[0].Instance,
			})
			if err != nil {
				if couchdb.IsConflictError(err) {
					c, err = contact.FindByEmail(inst, s.Members[0].Email)
					if err != nil {
						inst.Logger().WithNamespace("sharing").
							Debugf("Contact creation conflict and retry failed: %s", err)
						c = nil
					}
				} else {
					inst.Logger().WithNamespace("sharing").
						Warnf("Could not create contact for sender: %s", err)
				}
			} else {
				inst.Logger().WithNamespace("sharing").
					Infof("Created contact for sender %s", s.Members[0].Email)
			}
		}

		if c != nil && !c.IsTrusted() {
			if err := c.MarkAsTrusted(inst); err != nil {
				inst.Logger().WithNamespace("sharing").
					Warnf("Could not mark contact as trusted: %s", err)
			} else {
				inst.Logger().WithNamespace("sharing").
					Infof("Marked contact %s as trusted after accepting sharing", s.Members[0].Email)
			}
		}
	}

	return updateSharingWithConflictRetry(inst, s, 1, func(latest *Sharing) bool {
		if latest.Active {
			return true
		}
		latest.Members = s.Members
		latest.Credentials = s.Credentials
		latest.Active = s.Active
		latest.Initial = s.Initial
		if s.ShortcutID != "" {
			latest.ShortcutID = s.ShortcutID
		}
		return false
	})
}

// ProcessAnswer takes somes credentials and update the sharing with those.
func (s *Sharing) ProcessAnswer(inst *instance.Instance, creds *APICredentials) (*APICredentials, error) {
	if !s.Owner || len(s.Members) != len(s.Credentials)+1 {
		return nil, ErrInvalidSharing
	}
	if err := requireOAuthCredentials(creds); err != nil {
		return nil, err
	}
	if creds.State == "" {
		return nil, ErrInvalidSharing
	}
	for i, c := range s.Credentials {
		if c.State == creds.State {
			// Stage member changes on a local copy and only write them back
			// once OAuth client + access token are minted, so a failure mid-way
			// leaves the sharing untouched instead of partially mutated.
			member := s.Members[i+1]
			member.Status = MemberStatusReady
			member.PublicName = creds.PublicName
			ac := APICredentials{
				CID: s.SID,
				Credentials: &Credentials{
					XorKey: c.XorKey,
				},
			}
			// Create the credentials for the recipient
			cli, err := CreateOAuthClient(inst, &member)
			if err != nil {
				return nil, err
			}
			ac.Credentials.Client = ConvertOAuthClient(cli)
			var verb permission.VerbSet
			// In case of read-only, the recipient only needs read access on the
			// sharing, e.g. to notify the sharer of a revocation
			if s.ReadOnlyRules() || s.Members[i+1].ReadOnly {
				verb = permission.Verbs(permission.GET)
			} else {
				verb = permission.ALL
			}
			token, err := CreateAccessToken(inst, cli, s.SID, verb)
			if err != nil {
				return nil, err
			}
			ac.Credentials.AccessToken = token

			s.Members[i+1] = member
			s.Credentials[i].Client = creds.Client
			s.Credentials[i].AccessToken = creds.AccessToken
			s.Credentials[i].InboundClientID = cli.ClientID

			// Update the contact to fill the name if missing
			if email := s.Members[i+1].Email; email != "" {
				if c, err := contact.FindByEmail(inst, email); err == nil {
					if err := c.AddNameIfMissing(inst, s.Members[i+1].PublicName, email); err != nil {
						inst.Logger().WithNamespace("sharing").
							Warnf("Error on saving contact: %s", err)
					}
				}
			}

			s.Active = true
			if err := updateSharingWithConflictRetry(inst, s, 1, func(latest *Sharing) bool {
				if latest.Members[i+1].Status == MemberStatusReady &&
					latest.Credentials[i].Client != nil &&
					latest.Credentials[i].AccessToken != nil {
					ac.Credentials.Client = latest.Credentials[i].Client
					ac.Credentials.AccessToken = latest.Credentials[i].AccessToken
					return true
				}
				latest.Members[i+1] = s.Members[i+1]
				latest.Credentials[i] = s.Credentials[i]
				latest.Active = s.Active
				return false
			}); err != nil {
				return nil, err
			}
			if creds.Bitwarden != nil {
				if err := s.SaveBitwarden(inst, &s.Members[i+1], creds.Bitwarden); err != nil {
					return nil, err
				}
			}
			go s.Setup(inst, &s.Members[i+1])
			return &ac, nil
		}
	}
	return nil, ErrMemberNotFound
}

// ChangeOwnerAddress is used when the owner of the sharing has moved their
// instance to a new URL and the other members of the sharing are informed of
// the new URL.
func (s *Sharing) ChangeOwnerAddress(inst *instance.Instance, params APIMoved) error {
	s.Members[0].Instance = params.NewInstance
	s.Credentials[0].AccessToken.AccessToken = params.AccessToken
	s.Credentials[0].AccessToken.RefreshToken = params.RefreshToken
	updateContactAddress(inst, s.Members[0].Email, params.NewInstance)
	return couchdb.UpdateDoc(inst, s)
}

// ChangeMemberAddress is used when a recipient of the sharing has moved their
// instance to a new URL and the owner if informed of the new URL.
func (s *Sharing) ChangeMemberAddress(inst *instance.Instance, m *Member, params APIMoved) error {
	m.Instance = params.NewInstance
	for i := range s.Members {
		if i == 0 {
			continue
		}
		if m.Same(s.Members[i]) {
			s.Credentials[i-1].AccessToken.AccessToken = params.AccessToken
			s.Credentials[i-1].AccessToken.RefreshToken = params.RefreshToken
		}
	}
	updateContactAddress(inst, m.Email, params.NewInstance)
	return couchdb.UpdateDoc(inst, s)
}

func updateContactAddress(inst *instance.Instance, email, newInstance string) {
	if email == "" {
		return
	}
	c, err := contact.FindByEmail(inst, email)
	if err != nil {
		return
	}
	_ = c.ChangeCozyURL(inst, newInstance)
}

// RefreshToken is used after a failed request with a 4xx error code.
// It checks if the targeted instance has moved, and tries on the new instance
// if it is the case. And, if needed, it renews the access token and retries
// the request.
func RefreshToken(
	inst *instance.Instance,
	res *http.Response,
	reqErr error,
	s *Sharing,
	m *Member,
	creds *Credentials,
	opts *request.Options,
	body []byte,
) (*http.Response, error) {
	// Check if a redirect occurred (e.g., 302 redirect to new domain).
	// When Go's http.Client follows a redirect to a different host, it strips
	// the Authorization header for security. This causes 401 Unauthorized on
	// the new domain. We detect this by comparing the final URL with the original.
	// We only check on 401 to avoid false positives on other 4xx errors.
	if res != nil && res.StatusCode == http.StatusUnauthorized {
		if res.Request != nil && res.Request.URL != nil {
			finalHost := res.Request.URL.Host
			if finalHost != "" && finalHost != opts.Domain {
				// Redirect detected - update member instance to the new domain
				newURL := &url.URL{Scheme: res.Request.URL.Scheme, Host: finalHost}
				m.Instance = newURL.String()
				opts.Scheme = newURL.Scheme
				opts.Domain = newURL.Host
				inst.Logger().WithNamespace("sharing").
					Infof("Detected redirect from %s to %s, updating member instance", opts.Domain, m.Instance)
			}
		}
	}

	if err, ok := reqErr.(*request.Error); ok && err.Status == http.StatusText(http.StatusGone) {
		tryUpdateMemberInstance(err, m, opts)
	}

	if err := creds.Refresh(inst, s, m); err != nil {
		return nil, err
	}
	// Safety check: credentials may have been cleared by a concurrent revocation
	if creds.AccessToken == nil {
		return nil, ErrNoOAuthClient
	}
	opts.Headers["Authorization"] = "Bearer " + creds.AccessToken.AccessToken
	if body != nil {
		opts.Body = bytes.NewReader(body)
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 5 {
		return nil, ErrInternalServerError
	}
	return res, err
}

func tryUpdateMemberInstance(reqErr *request.Error, m *Member, opts *request.Options) {
	m.Instance = reqErr.Title
	u, err := url.Parse(m.Instance)
	if err != nil {
		return
	}
	opts.Scheme = u.Scheme
	opts.Domain = u.Host
}

// ParseRequestError is used to parse an error in a request.Options, and it
// keeps the new instance URL when a Cozy has moved in Title.
func ParseRequestError(res *http.Response, body []byte) error {
	if res.StatusCode != http.StatusGone {
		return &request.Error{
			Status: http.StatusText(res.StatusCode),
			Title:  http.StatusText(res.StatusCode),
			Detail: string(body),
		}
	}

	var errors struct {
		List jsonapi.ErrorList `json:"errors"`
	}
	if err := json.Unmarshal(body, &errors); err != nil {
		return &request.Error{
			Status: http.StatusText(res.StatusCode),
			Title:  http.StatusText(res.StatusCode),
			Detail: string(body),
		}
	}
	var newInstance string
	if len(errors.List) == 1 && errors.List[0].Links != nil && errors.List[0].Links.Related != "" {
		newInstance = errors.List[0].Links.Related
	}
	return &request.Error{
		Status: http.StatusText(res.StatusCode),
		Title:  newInstance,
		Detail: string(body),
	}
}

// TryTokenForMovedSharing is used when a Cozy has been moved, and a sharing
// was not updated on the other Cozy for some reasons. When the other Cozy will
// try to make a request to the source Cozy, it will get a 410 Gone error. This
// error will also tell it the URL of the new Cozy. Thus, it can try to refresh
// the token on the destination Cozy. And, as the refresh token was emitted on
// the source Cozy (and not the target Cozy), we need to do some tricks to
// manage this refresh. This function is here for that.
func TryTokenForMovedSharing(i *instance.Instance, c *oauth.Client, token string) (string, permission.Claims, bool) {
	// Extract the sharing ID from the scope of the refresh token
	claims := permission.Claims{}
	if token == "" {
		return "", claims, false
	}
	_, _, err := new(jwt.Parser).ParseUnverified(token, &claims)
	if err != nil {
		return "", claims, false
	}
	parts := strings.Split(claims.Scope, ":")
	if len(parts) != 3 || parts[0] != consts.Sharings {
		return "", claims, false
	}

	// Find the sharing and check that it has been moved from another instance
	s, err := FindSharing(i, parts[2])
	if err != nil || s.MovedFrom == "" {
		return "", claims, false
	}
	validUntil := s.UpdatedAt.Add(consts.AccessTokenValidityDuration)
	if validUntil.Before(time.Now().UTC()) {
		// This trick is only accepted in the week following the move, not after
		return "", claims, false
	}

	// Call the other instance and check the response
	q := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {token},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
	}
	if c.ClientID == "" {
		q.Set("client_id", c.CouchID)
	}
	payload := strings.NewReader(q.Encode())
	req, err := http.NewRequest("POST", s.MovedFrom+"/auth/access_token", payload)
	if err != nil {
		return "", claims, false
	}
	req.Header.Add(echo.HeaderContentType, echo.MIMEApplicationForm)
	req.Header.Add(echo.HeaderAccept, echo.MIMEApplicationJSON)
	res, err := safehttp.ClientWithKeepAlive.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		return "", claims, false
	}
	defer res.Body.Close()
	body := &auth.AccessToken{}
	if err = json.NewDecoder(res.Body).Decode(&body); err != nil || body.AccessToken == "" {
		return "", claims, false
	}
	other := permission.Claims{}
	_, _, err = new(jwt.Parser).ParseUnverified(body.AccessToken, &other)
	if err != nil {
		return "", claims, false
	}

	// Create a new refresh token
	refresh, err := c.CreateJWT(i, consts.RefreshTokenAudience, claims.Scope)
	return refresh, claims, err == nil
}
