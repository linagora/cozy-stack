package sharing

import (
	"context"
	"errors"
	"net/http"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"golang.org/x/sync/errgroup"
)

// RevocationWorker notifies recipients after owner-side revocation.
const RevocationWorker = "share-revoke"

const revocationBatchSize = 50

// RevocationJob protects notification credentials from readers of the jobs API.
type RevocationJob struct {
	Encrypted string `json:"encrypted"`
}

// RevocationMsg keeps only the outgoing credentials needed after owner cleanup.
// It is encrypted before being stored in a job, whose message is exposed by the jobs API.
type RevocationMsg struct {
	SharingID  string                `json:"sharing_id"`
	Recipients []RevocationRecipient `json:"recipients"`
}

type RevocationRecipient struct {
	Member      Member      `json:"member"`
	Credentials Credentials `json:"credentials,omitempty"`
	Token       string      `json:"token"`
}

func (s *Sharing) enqueueRevocations(inst *instance.Instance) error {
	msg := RevocationMsg{SharingID: s.SID}
	push := func() error {
		encrypted, err := account.EncryptCredentialsData(&msg)
		if err != nil {
			return err
		}
		body, err := job.NewMessage(&RevocationJob{Encrypted: encrypted})
		if err != nil {
			return err
		}
		_, err = job.System().PushJob(inst, &job.JobRequest{WorkerType: RevocationWorker, Message: body})
		return err
	}
	for i := 1; i < len(s.Members); i++ {
		m, c := &s.Members[i], &s.Credentials[i-1]
		if m.Status == MemberStatusRevoked || m.Instance == "" || (!s.Drive && m.Status != MemberStatusReady) {
			continue
		}
		token, err := s.revocationToken(inst, m, c)
		if errors.Is(err, ErrNoOAuthClient) || couchdb.IsNotFoundError(err) {
			inst.Logger().WithNamespace("sharing").Warnf("Cannot notify %s of revocation: %s", m.Instance, err)
			continue
		}
		if err != nil {
			return err
		}
		if token == "" {
			continue
		}
		msg.Recipients = append(msg.Recipients, RevocationRecipient{
			Member:      *m,
			Credentials: Credentials{Client: c.Client, AccessToken: c.AccessToken},
			Token:       token,
		})
		if len(msg.Recipients) == revocationBatchSize {
			if err := push(); err != nil {
				return err
			}
			msg.Recipients = nil
		}
	}
	if len(msg.Recipients) > 0 {
		return push()
	}
	return nil
}

// NotifyRevocations retries transient failures without updating the owner's sharing.
func NotifyRevocations(ctx context.Context, inst *instance.Instance, msg *RevocationMsg) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i := range msg.Recipients {
		r := &msg.Recipients[i]
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// shortcut: refreshed credentials last one attempt; persist them if refresh tokens become single-use.
			err := notifyMemberRevocation(inst, msg.SharingID, &r.Member, &r.Credentials, r.Token)
			var reqErr *request.Error
			if errors.As(err, &reqErr) {
				switch reqErr.Status {
				case http.StatusText(http.StatusNotFound), http.StatusText(http.StatusUnauthorized), http.StatusText(http.StatusForbidden):
					// A replay can reach a recipient that already removed the sharing or its credentials.
					return nil
				}
			}
			var authErr *auth.Error
			if errors.As(err, &authErr) && (authErr.Value == "invalid_grant" || authErr.Value == "invalid_client" || authErr.Value == "the client must be registered") {
				return nil
			}
			return err
		})
	}
	return g.Wait()
}
