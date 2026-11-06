package session

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/labstack/echo/v4"
)

// SessionMaxAge is the maximum duration of the session in seconds
const SessionMaxAge = 30 * 24 * time.Hour

// defaultCookieName is name of the cookie created by cozy on nested subdomains
const defaultCookieName = "cozysessid"

var (
	// ErrNoCookie is returned by GetSession if there is no cookie
	ErrNoCookie = errors.New("No session cookie")
	// ErrExpired is returned when the session has expired
	ErrExpired = errors.New("Session expired")
	// ErrInvalidID is returned by GetSession if the cookie contains wrong ID
	ErrInvalidID = errors.New("Session cookie has wrong ID")
)

// A Session is an instance opened in a browser
type Session struct {
	instance  *instance.Instance
	DocID     string    `json:"_id,omitempty"`
	DocRev    string    `json:"_rev,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	LongRun   bool      `json:"long_run"`
	ShortRun  bool      `json:"short_run"`
	SID       string    `json:"sid,omitempty"` // only present with OIDC
	// OIDCProviderKey namespaces the upstream sid. In the first iteration it is
	// simply the instance context name.
	OIDCProviderKey string `json:"oidc_provider_key,omitempty"`
}

// DocType implements couchdb.Doc
func (s *Session) DocType() string { return consts.Sessions }

// ID implements couchdb.Doc
func (s *Session) ID() string { return s.DocID }

// SetID implements couchdb.Doc
func (s *Session) SetID(v string) { s.DocID = v }

// Rev implements couchdb.Doc
func (s *Session) Rev() string { return s.DocRev }

// SetRev implements couchdb.Doc
func (s *Session) SetRev(v string) { s.DocRev = v }

// Clone implements couchdb.Doc
func (s *Session) Clone() couchdb.Doc {
	cloned := *s
	if cloned.instance != nil {
		tmp := *s.instance
		cloned.instance = &tmp
	}
	return &cloned
}

// ensure Session implements couchdb.Doc
var _ couchdb.Doc = (*Session)(nil)

// Duration is a type for the cookie expiration.
type Duration int

const (
	// ShortRun is used for session that will last only 5 minutes. It is
	// typically used for OAuth dance.
	ShortRun Duration = iota
	// NormalRun is used for a session that will expired when the browser is
	// closed.
	NormalRun
	// LongRun is used to try to keep the session opened as long as possible.
	LongRun
)

// BelongsTo reports whether the session was loaded for the given instance.
func (s *Session) BelongsTo(inst *instance.Instance) bool {
	return s.instance != nil && inst != nil && s.instance.Domain == inst.Domain
}

// Duration returns the session duration for the its cookie.
func (s *Session) Duration() Duration {
	if s.LongRun {
		return LongRun
	} else if s.ShortRun {
		return ShortRun
	}
	return NormalRun
}

// OlderThan checks if a session last seen is older than t from now
func (s *Session) OlderThan(t time.Duration) bool {
	return time.Now().After(s.LastSeen.Add(t))
}

// New creates a session in couchdb for the given instance
func New(i *instance.Instance, duration Duration, sid string) (*Session, error) {
	now := time.Now()
	s := &Session{
		instance:  i,
		LastSeen:  now,
		CreatedAt: now,
		ShortRun:  duration == ShortRun,
		LongRun:   duration == LongRun,
		SID:       sid,
	}
	if err := couchdb.CreateDoc(i, s); err != nil {
		return nil, err
	}
	if sid != "" {
		s.OIDCProviderKey = i.ContextName
		bindOIDCSession(i, s)
	}
	return s, nil
}

func lockSession(inst *instance.Instance, sessionID string) func() {
	mu := config.Lock().ReadWrite(inst, "sessions/"+sessionID)
	_ = mu.Lock()
	return mu.Unlock
}

// Get fetches the session
func Get(i *instance.Instance, sessionID string) (*Session, error) {
	s := &Session{}
	err := couchdb.GetDoc(i, consts.Sessions, sessionID, s)
	if couchdb.IsNotFoundError(err) {
		return nil, ErrInvalidID
	}
	if err != nil {
		return nil, err
	}
	s.instance = i

	// If the session is older than the session max age, it has expired and
	// should be deleted.
	if s.OlderThan(SessionMaxAge) {
		defer lockSession(i, sessionID)()
		err := couchdb.DeleteDoc(i, s)
		if err != nil {
			i.Logger().WithNamespace("loginaudit").
				Warnf("Failed to delete expired session: %s", err)
		} else {
			i.Logger().WithNamespace("loginaudit").
				Infof("Expired session deleted: %s", s.DocID)
			unbindOIDCSession(i, s)
		}
		return nil, ErrExpired
	}

	// In order to avoid too many updates of the session document, we have an
	// update period of one day for the `last_seen` date, which is a good enough
	// granularity.
	if s.OlderThan(24 * time.Hour) {
		defer lockSession(i, sessionID)()
		lastSeen := s.LastSeen
		s.LastSeen = time.Now()
		err := couchdb.UpdateDoc(i, s)
		if err != nil {
			s.LastSeen = lastSeen
		} else {
			touchOIDCSession(i, s)
		}
	}

	return s, nil
}

// CookieName returns the name of the cookie used for the given instance.
func CookieName(i *instance.Instance) string {
	if config.GetConfig().Subdomains == config.FlatSubdomains {
		return "sess-" + i.DBPrefix()
	}
	return defaultCookieName
}

// ForceHostOnlyCookies can be set to true in tests to make cookies host-only.
// This is needed because test HTTP clients connect to 127.0.0.1, not to the
// instance domain, so domain-scoped cookies would not be sent back.
var ForceHostOnlyCookies bool

// CookieDomain returns the domain on which the cookie will be set. On nested
// subdomains, the cookie is put on the domain of the instance, but for flat
// subdomains, we need to put it one level higher (eg .mycozy.cloud instead of
// .example.mycozy.cloud) to make the cookie available when the user visits
// their apps.
func CookieDomain(i *instance.Instance) string {
	if ForceHostOnlyCookies {
		return ""
	}
	domain := i.ContextualDomain()
	if config.GetConfig().Subdomains == config.FlatSubdomains {
		parts := strings.SplitN(domain, ".", 2)
		if len(parts) > 1 {
			domain = parts[1]
		}
	}
	return utils.CookieDomain("." + domain)
}

// FromCookie retrieves the session from a echo.Context cookies.
func FromCookie(c echo.Context, i *instance.Instance) (*Session, error) {
	cookie, err := c.Cookie(CookieName(i))
	if err != nil || cookie.Value == "" {
		return nil, ErrNoCookie
	}

	sessionID, err := crypto.DecodeAuthMessage(cookieSessionMACConfig(i), i.SessionSecret(),
		[]byte(cookie.Value), nil)
	if err != nil {
		return nil, err
	}

	return Get(i, string(sessionID))
}

// GetAll returns all the active sessions
func GetAll(inst *instance.Instance) ([]*Session, error) {
	var sessions []*Session
	req := couchdb.AllDocsRequest{
		Limit: 50000,
	}
	if err := couchdb.GetAllDocs(inst, consts.Sessions, &req, &sessions); err != nil {
		return nil, err
	}
	var expired []couchdb.Doc
	kept := sessions[:0]
	for _, sess := range sessions {
		sess.instance = inst
		if sess.OlderThan(SessionMaxAge) {
			expired = append(expired, sess)
		} else {
			kept = append(kept, sess)
		}
	}
	if len(expired) > 0 {
		if err := couchdb.BulkDeleteDocs(inst, consts.Sessions, expired); err != nil {
			inst.Logger().WithNamespace("sessions").
				Infof("Error while deleting expired sessions: %s", err)
		} else {
			for _, doc := range expired {
				sess, ok := doc.(*Session)
				if ok {
					unbindOIDCSession(inst, sess)
				}
			}
		}
	}
	return kept, nil
}

// Delete is a function to delete the session in couchdb,
// and returns a cookie with a negative MaxAge to clear it
func (s *Session) Delete(i *instance.Instance) *http.Cookie {
	err := couchdb.DeleteDoc(i, s)
	if err != nil {
		i.Logger().WithNamespace("loginaudit").
			Errorf("Failed to delete session: %s", err)
	} else {
		i.Logger().WithNamespace("loginaudit").
			Infof("Session deleted: %s", s.DocID)
		unbindOIDCSession(i, s)
	}
	return &http.Cookie{
		Name:   CookieName(i),
		Value:  "",
		MaxAge: -1,
		Path:   "/",
		Domain: CookieDomain(i),
	}
}

// ToCookie returns an http.Cookie for this Session
func (s *Session) ToCookie() (*http.Cookie, error) {
	inst := s.instance
	encoded, err := crypto.EncodeAuthMessage(cookieSessionMACConfig(inst), inst.SessionSecret(), []byte(s.ID()), nil)
	if err != nil {
		return nil, err
	}

	maxAge := 0
	if s.LongRun {
		maxAge = 10 * 365 * 24 * 3600 // 10 years
	} else if s.ShortRun {
		maxAge = 5 * 60 // 5 minutes
	}

	return &http.Cookie{
		Name:     CookieName(inst),
		Value:    string(encoded),
		MaxAge:   maxAge,
		Path:     "/",
		Domain:   CookieDomain(inst),
		Secure:   !build.IsDevRelease(),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}, nil
}

// DeleteOthers will remove all sessions except the one given in parameter.
func DeleteOthers(i *instance.Instance, selfSessionID string) error {
	var sessions []*Session
	err := couchdb.ForeachDocs(i, consts.Sessions, func(_ string, data json.RawMessage) error {
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		sessions = append(sessions, &s)
		return nil
	})
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.ID() != selfSessionID {
			s.Delete(i)
		}
	}
	return nil
}

// DeleteBySID is used for the OIDC back-channel logout. It deletes the sessions
// for the current device of the user.
func DeleteBySID(inst *instance.Instance, sid string) error {
	return couchdb.ForeachDocs(inst, consts.Sessions, func(_ string, data json.RawMessage) error {
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if s.SID == sid {
			s.Delete(inst)
		}
		return nil
	})
}

// cookieSessionMACConfig returns the options to authenticate the session
// cookie.
//
// We rely on a MACed cookie value, without additional encryption of the
// message since it should not contain critical private informations and is
// protected by HTTPs (secure cookie).
//
// About MaxLength, for a session of size 100 bytes
//
//	    8 bytes time
//	+  32 bytes HMAC-SHA256
//	+ 100 bytes session
//	+ base64 encoding (4*n/3)
//	< 200 bytes
//
// 256 bytes should be sufficient enough to support any type of session.
func cookieSessionMACConfig(i *instance.Instance) crypto.MACConfig {
	return crypto.MACConfig{
		Name:   CookieName(i),
		MaxLen: 256,
	}
}
