package sharing

import (
	"net/http"
	"net/url"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	multierror "github.com/hashicorp/go-multierror"
)

func init() {
	lifecycle.AskReupload = AskReupload
}

// AskReupload is used when the disk quota of an instance is increased to tell
// to the other instances that have a sharing with it that they can retry to
// upload files.
func AskReupload(inst *instance.Instance) error {
	// XXX If there are more than 100 sharings, it is probably better to rely
	// on the existing retry mechanism than asking for all of the sharings, as
	// it may slow down the sharings.
	req := &couchdb.AllDocsRequest{
		Limit: 100,
	}
	var sharings []*Sharing
	err := couchdb.GetAllDocs(inst, consts.Sharings, req, &sharings)
	if err != nil {
		return err
	}
	var errm error
	for _, s := range sharings {
		if !s.Active || s.FirstFilesRule() == nil {
			continue
		}
		if s.Owner {
			for i, m := range s.Members {
				if i == 0 {
					continue // skip the owner
				}
				if m.Status != MemberStatusReady {
					continue
				}
				if err := askReuploadTo(inst, s, &s.Members[i], &s.Credentials[i-1]); err != nil {
					errm = multierror.Append(errm, err)
				}
			}

			continue
		}

		if len(s.Credentials) > 0 {
			if err := askReuploadTo(inst, s, &s.Members[0], &s.Credentials[0]); err != nil {
				errm = multierror.Append(errm, err)
			}
		}
	}
	return errm
}

func askReuploadTo(inst *instance.Instance, s *Sharing, m *Member, c *Credentials) error {
	if c == nil || c.AccessToken == nil {
		return nil
	}
	u, err := url.Parse(m.Instance)
	if err != nil {
		return err
	}
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/reupload",
		Headers: request.Headers{
			"Authorization": "Bearer " + c.AccessToken.AccessToken,
		},
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && ShouldRefreshToken(res.StatusCode) {
		res, err = RefreshToken(inst, res, err, s, m, c, opts, nil)
	}
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return nil
}

// PushUploadJob pushs a job for the share-upload worker, to try again to
// reupload files.
func PushUploadJob(s *Sharing, inst *instance.Instance) {
	if s.Active && s.FirstFilesRule() != nil {
		s.pushJob(inst, "share-upload")
	}
}
