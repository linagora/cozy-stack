package sharing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/revision"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	"github.com/labstack/echo/v4"
	"golang.org/x/sync/errgroup"
)

// MaxRetries is the maximal number of retries for a replicator
const MaxRetries = 5

// InitialBackoffPeriod is the initial duration to wait for the first retry
// (each next retry will wait 4 times longer than its previous retry)
const InitialBackoffPeriod = 1 * time.Minute

// BatchSize is the maximal number of documents manipulated at once by the
// replicator
const BatchSize = 400

// ReplicateMsg is used for jobs on the share-replicate worker.
type ReplicateMsg struct {
	SharingID string `json:"sharing_id"`
	Errors    int    `json:"errors"`
}

// UpdateMsg is used for jobs on the share-update worker.
type UpdateMsg struct {
	SharingID      string `json:"sharing_id"`
	NewDescription string `json:"new_description"`
}

// Replicate starts a replicator on this sharing.
func (s *Sharing) Replicate(inst *instance.Instance, errorsCount int) error {
	mu := config.Lock().ReadWrite(inst, "sharings/"+s.SID)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	pending := false
	var err error
	if !s.Owner {
		pending, err = s.ReplicateTo(inst, &s.Members[0], false)
	} else {
		g, _ := errgroup.WithContext(context.Background())
		for i := range s.Members {
			if i == 0 {
				continue
			}
			m := &s.Members[i]
			g.Go(func() error {
				defer func() {
					if r := recover(); r != nil {
						inst.Logger().Errorf("[panic] %v: %s", r, debug.Stack())
					}
				}()
				if m.Status == MemberStatusReady {
					p, err := s.ReplicateTo(inst, m, false)
					if err != nil {
						return err
					}
					if p {
						pending = true
					}
				}
				return nil
			})
		}
		err = g.Wait()
	}
	if err != nil {
		if errors.Is(err, errRevokeSharing) {
			if s.Owner {
				return s.Revoke(inst)
			}
			return s.RevokeRecipientBySelf(inst, false)
		}
		s.retryWorker(inst, "share-replicate", errorsCount)
	} else if pending {
		s.pushJob(inst, "share-replicate")
	}
	return err
}

// pushJob adds a new job to continue on the pending documents in the changes feed
func (s *Sharing) pushJob(inst *instance.Instance, worker string) {
	inst.Logger().WithNamespace("replicator").
		Debugf("Push a new job for worker %s for sharing %s", worker, s.SID)
	msg, err := job.NewMessage(&ReplicateMsg{
		SharingID: s.SID,
		Errors:    0,
	})
	if err != nil {
		inst.Logger().WithNamespace("replicator").
			Warnf("Error on push job to %s: %s", worker, err)
		return
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: worker,
		Message:    msg,
	})
	if err != nil {
		inst.Logger().WithNamespace("replicator").
			Warnf("Error on push job to %s: %s", worker, err)
		return
	}
}

// retryWorker will add a job to retry a failed replication or upload
func (s *Sharing) retryWorker(inst *instance.Instance, worker string, errors int) {
	inst.Logger().WithNamespace("replicator").
		Debugf("Retry worker %s for sharing %s", worker, s.SID)
	backoff := InitialBackoffPeriod << uint(errors*2)
	errors++
	if errors == MaxRetries {
		inst.Logger().WithNamespace("replicator").Warnf("Max retries reached")
		return
	}
	msg, err := job.NewMessage(&ReplicateMsg{
		SharingID: s.SID,
		Errors:    errors,
	})
	if err != nil {
		inst.Logger().WithNamespace("replicator").
			Warnf("Error on retry to %s: %s", worker, err)
		return
	}
	t, err := job.NewTrigger(inst, job.TriggerInfos{
		Type:       "@in",
		WorkerType: worker,
		Arguments:  backoff.String(),
	}, msg)
	if err != nil {
		inst.Logger().WithNamespace("replicator").
			Warnf("Error on retry to %s: %s", worker, err)
		return
	}
	if err = job.System().AddTrigger(t); err != nil {
		inst.Logger().WithNamespace("replicator").
			Warnf("Error on retry to %s: %s", worker, err)
	}
}

func (s *Sharing) InitialReplication(inst *instance.Instance, m *Member) error {
	for i := 0; i < 1000; i++ {
		pending, err := s.ReplicateTo(inst, m, true)
		if err != nil {
			return err
		}
		if !pending {
			return nil
		}
	}
	return ErrInternalServerError
}

// ReplicateTo starts a replicator on this sharing to the given member.
// http://docs.couchdb.org/en/stable/replication/protocol.html
// https://github.com/pouchdb/pouchdb/blob/master/packages/node_modules/pouchdb-replication/src/replicate.js
func (s *Sharing) ReplicateTo(inst *instance.Instance, m *Member, initial bool) (bool, error) {
	if m.Instance == "" {
		return false, ErrInvalidURL
	}
	creds := s.FindCredentials(m)
	if creds == nil {
		return false, ErrInvalidSharing
	}

	lastSeq, err := s.getLastSeqNumber(inst, m, "replicator")
	if err != nil {
		return false, err
	}
	inst.Logger().WithNamespace("replicator").Debugf("lastSeq = %s", lastSeq)

	feed, err := s.callChangesFeed(inst, lastSeq)
	if err != nil {
		return false, err
	}
	if feed.Seq == lastSeq {
		return false, nil
	}
	inst.Logger().WithNamespace("replicator").Debugf("changes = %#v", feed.Changes)

	changes := &feed.Changes
	if len(changes.Changed) > 0 {
		var missings *Missings
		if initial || len(changes.Changed) == len(changes.Removed) {
			missings = transformChangesInMissings(changes)
		} else {
			missings, err = s.callRevsDiff(inst, m, creds, changes)
			if err != nil {
				return false, err
			}
		}
		inst.Logger().WithNamespace("replicator").Debugf("missings = %#v", missings)

		docs, errb := s.getMissingDocs(inst, missings, changes)
		if errb != nil {
			return false, errb
		}
		inst.Logger().WithNamespace("replicator").Debugf("docs = %#v", docs)

		err = s.sendBulkDocs(inst, m, creds, docs, feed.RuleIndexes)
		if err != nil {
			return false, err
		}
	}

	err = s.UpdateLastSequenceNumber(inst, m, "replicator", feed.Seq)
	return feed.Pending, err
}

// getLastSeqNumber returns the last sequence number of the previous
// replication to this member
func (s *Sharing) getLastSeqNumber(inst *instance.Instance, m *Member, worker string) (string, error) {
	id, err := s.replicationID(m)
	if err != nil {
		return "", err
	}
	result, err := couchdb.GetLocal(inst, consts.Shared, id+"/"+worker)
	if couchdb.IsNotFoundError(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	seq, _ := result["last_seq"].(string)
	return seq, nil
}

// UpdateLastSequenceNumber updates the last sequence number for this
// replication if it's superior to the number in CouchDB
func (s *Sharing) UpdateLastSequenceNumber(inst *instance.Instance, m *Member, worker, seq string) error {
	id, err := s.replicationID(m)
	if err != nil {
		return err
	}
	result, err := couchdb.GetLocal(inst, consts.Shared, id+"/"+worker)
	if err != nil {
		if !couchdb.IsNotFoundError(err) {
			return err
		}
		result = make(map[string]interface{})
	} else {
		if prev, ok := result["last_seq"].(string); ok {
			if revision.Generation(seq) <= revision.Generation(prev) {
				return nil
			}
		}
	}
	result["last_seq"] = seq
	return couchdb.PutLocal(inst, consts.Shared, id+"/"+worker, result)
}

// ClearLastSequenceNumbers removes the last sequence numbers for a member
func (s *Sharing) ClearLastSequenceNumbers(inst *instance.Instance, m *Member) error {
	errr := s.clearLastSequenceNumber(inst, m, "replicator")
	erru := s.clearLastSequenceNumber(inst, m, "upload")
	if errr != nil {
		return errr
	}
	return erru
}

// clearLastSequenceNumber removes a last sequence number for a member on a given worker
func (s *Sharing) clearLastSequenceNumber(inst *instance.Instance, m *Member, worker string) error {
	id, err := s.replicationID(m)
	if err != nil {
		return err
	}
	err = couchdb.DeleteLocal(inst, consts.Shared, id+"/"+worker)
	if couchdb.IsNotFoundError(err) {
		return nil
	}
	return err
}

// replicationID gives an identifier for this replicator
func (s *Sharing) replicationID(m *Member) (string, error) {
	for i := range s.Members {
		if &s.Members[i] == m {
			id := fmt.Sprintf("sharing-%s-%d", s.SID, i)
			return id, nil
		}
	}
	return "", ErrMemberNotFound
}

// Changed is a map of "doctype/docid" -> [revisions]
type Changed map[string][]string

// Removed is a set of "doctype/docid"
type Removed map[string]struct{}

// Changes is a struct with informations from the changes feed of io.cozy.shared
type Changes struct {
	Changed Changed
	Removed Removed
}

func extractLastRevision(doc couchdb.JSONDoc) string {
	var rev string
	subtree := doc.Get("revisions")
	for {
		m, ok := subtree.(map[string]interface{})
		if !ok {
			break
		}
		rev = m["rev"].(string)
		branches, ok := m["branches"].([]interface{})
		if !ok || len(branches) == 0 {
			break
		}
		subtree = branches[0]
	}
	return rev
}

func extractRevisionsSlice(doc couchdb.JSONDoc) []string {
	slice := []string{}
	subtree := doc.Get("revisions")
	for {
		m, ok := subtree.(map[string]interface{})
		if !ok {
			break
		}
		rev := m["rev"].(string)
		slice = append(slice, rev)
		branches, ok := m["branches"].([]interface{})
		if !ok || len(branches) == 0 {
			break
		}
		subtree = branches[0]
	}
	return slice
}

// changesResponse contains the useful informations from a call to the changes
// feed in the replicator context
type changesResponse struct {
	// Changes is the list of changed documents
	Changes Changes
	// RuleIndexes is a mapping between the "doctype/docid" -> rule index
	RuleIndexes map[string]int
	// Seq is the sequence number after these changes
	Seq string
	// Pending is true if there are some other changes in the feed after those
	Pending bool
}

// errRevokeSharing is a sentinel value that can be returned by callChangesFeed.
// It means that a document fetched from the changes that has been removed, and
// the sharing rule for it has the revoke action. So, the caller should check
// for this error, and it is the case, it should revoke the sharing.
var errRevokeSharing = errors.New("Sharing must be revoked")

// callChangesFeed fetches the last changes from the changes feed
// http://docs.couchdb.org/en/stable/api/database/changes.html
func (s *Sharing) callChangesFeed(inst *instance.Instance, since string) (*changesResponse, error) {
	response, err := couchdb.GetChanges(inst, &couchdb.ChangesRequest{
		DocType:     consts.Shared,
		IncludeDocs: true,
		Since:       since,
		Limit:       BatchSize,
	})
	if err != nil {
		return nil, err
	}
	res := changesResponse{
		Changes: Changes{
			Changed: make(Changed),
			Removed: make(Removed),
		},
		RuleIndexes: make(map[string]int),
		Seq:         response.LastSeq,
		Pending:     response.Pending > 0,
	}
	for _, r := range response.Results {
		infos, ok := r.Doc.Get("infos").(map[string]interface{})
		if !ok {
			continue
		}
		info, ok := infos[s.SID].(map[string]interface{})
		if !ok {
			continue
		}
		idx, ok := info["rule"].(float64)
		if !ok {
			continue
		}
		res.RuleIndexes[r.DocID] = int(idx)
		if _, ok = info["removed"]; ok {
			rule := s.Rules[int(idx)]
			if rule.Remove == ActionRuleRevoke {
				return nil, errRevokeSharing
			}
			res.Changes.Removed[r.DocID] = struct{}{}
		}
		if strings.HasPrefix(r.DocID, consts.Files+"/") {
			if rev := extractLastRevision(r.Doc); rev != "" {
				res.Changes.Changed[r.DocID] = []string{rev}
			}
		} else {
			res.Changes.Changed[r.DocID] = extractRevisionsSlice(r.Doc)
		}
	}
	return &res, nil
}

// Missings is a struct for the response of _revs_diff
type Missings map[string]MissingEntry

// MissingEntry is a struct with the missing revisions for an id
type MissingEntry struct {
	Missing []string `json:"missing"`
}

// transformChangesInMissings is used for the initial replication (revs_diff is
// not called), to prepare the payload for the bulk_get calls
func transformChangesInMissings(changes *Changes) *Missings {
	missings := make(Missings, len(changes.Changed))
	for key, revs := range changes.Changed {
		missings[key] = MissingEntry{
			Missing: []string{revs[len(revs)-1]},
		}
	}
	return &missings
}

// callRevsDiff asks the other cozy to compute the _revs_diff.
// This function does the ID transformation for files in both ways.
// http://docs.couchdb.org/en/stable/api/database/misc.html#db-revs-diff
func (s *Sharing) callRevsDiff(inst *instance.Instance, m *Member, creds *Credentials, changes *Changes) (*Missings, error) {
	u, err := url.Parse(m.Instance)
	if err != nil {
		return nil, err
	}
	// "io.cozy.files/docid" for recipient -> "io.cozy.files/docid" for sender
	xored := make(map[string]string)
	// "doctype/docid" -> [leaf revisions]
	leafRevs := make(Changed, len(changes.Changed))
	for key, revs := range changes.Changed {
		if len(creds.XorKey) > 0 {
			old := key
			parts := strings.SplitN(key, "/", 2)
			parts[1] = XorID(parts[1], creds.XorKey)
			key = parts[0] + "/" + parts[1]
			xored[key] = old
			leafRevs[key] = revs[len(revs)-1:]
		} else {
			leafRevs[key] = revs
		}
	}
	body, err := json.Marshal(leafRevs)
	if err != nil {
		return nil, err
	}
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/_revs_diff",
		Headers: request.Headers{
			echo.HeaderAccept:        echo.MIMEApplicationJSON,
			echo.HeaderContentType:   echo.MIMEApplicationJSON,
			echo.HeaderAuthorization: "Bearer " + creds.AccessToken.AccessToken,
		},
		Body:       bytes.NewReader(body),
		ParseError: ParseRequestError,
	}
	var res *http.Response
	res, err = request.Req(opts)
	if res != nil && ShouldRefreshToken(res.StatusCode) {
		res, err = RefreshToken(inst, res, err, s, m, creds, opts, body)
	}
	if err != nil {
		if res != nil && res.StatusCode/100 == 5 {
			return nil, ErrInternalServerError
		}
		return nil, err
	}
	defer res.Body.Close()

	missings := make(Missings)
	if err = json.NewDecoder(res.Body).Decode(&missings); err != nil {
		return nil, err
	}
	for k, v := range missings {
		if old, ok := xored[k]; ok {
			missings[old] = v
			delete(missings, k)
		}
	}
	return &missings, nil
}

// ComputeRevsDiff takes a map of id->[revisions] and returns the missing
// revisions for those documents on the current instance.
func (s *Sharing) ComputeRevsDiff(inst *instance.Instance, changed Changed) (*Missings, error) {
	inst.Logger().WithNamespace("replicator").
		Debugf("ComputeRevsDiff %#v", changed)
	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	results := make([]SharedRef, 0, len(changed))
	req := couchdb.AllDocsRequest{Keys: ids}
	err := couchdb.GetAllDocs(inst, consts.Shared, &req, &results)
	if err != nil {
		return nil, err
	}
	missings := make(Missings)
	for id, revs := range changed {
		missings[id] = MissingEntry{Missing: revs}
	}
	for _, result := range results {
		if _, ok := changed[result.SID]; !ok {
			continue
		}
		if _, ok := result.Infos[s.SID]; !ok {
			continue
		}
		notFounds := changed[result.SID][:0]
		for _, r := range changed[result.SID] {
			if sub, _ := result.Revisions.Find(r); sub == nil {
				notFounds = append(notFounds, r)
			}
		}
		if len(notFounds) == 0 {
			delete(missings, result.SID)
		} else {
			missings[result.SID] = MissingEntry{
				Missing: notFounds,
			}
		}
	}
	return &missings, nil
}

// DocsList is a slice of raw documents
type DocsList []map[string]interface{}

// DocsByDoctype is a map of doctype -> slice of documents of this doctype
type DocsByDoctype map[string]DocsList

// getMissingDocs fetches the documents in bulk, partitionned by their doctype.
// https://github.com/apache/couchdb-documentation/pull/263/files
func (s *Sharing) getMissingDocs(inst *instance.Instance, missings *Missings, changes *Changes) (*DocsByDoctype, error) {
	docs := make(DocsByDoctype)
	queries := make(map[string][]couchdb.IDRev) // doctype -> payload for _bulk_get
	for key, missing := range *missings {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 {
			return nil, ErrInternalServerError
		}
		doctype := parts[0]
		if _, ok := changes.Removed[key]; ok {
			revisions := changes.Changed[key]
			docs[doctype] = append(docs[doctype], map[string]interface{}{
				"_id":        parts[1],
				"_rev":       revisions[len(revisions)-1],
				"_revisions": revsChainToStruct(revisions),
				"_deleted":   true,
			})
			continue
		}
		for _, rev := range missing.Missing {
			ir := couchdb.IDRev{ID: parts[1], Rev: rev}
			queries[doctype] = append(queries[doctype], ir)
		}
	}

	for doctype, query := range queries {
		results, err := couchdb.BulkGetDocs(inst, doctype, query)
		if err != nil {
			return nil, err
		}
		docs[doctype] = append(docs[doctype], results...)
	}
	return &docs, nil
}

// sendBulkDocs takes a bulk of documents and send them to the other cozy.
// This function does the id and dir_id transformation before sending files.
// http://docs.couchdb.org/en/stable/api/database/bulk-api.html#db-bulk-docs
// https://wiki.apache.org/couchdb/HTTP_Bulk_Document_API#Posting_Existing_Revisions
// https://gist.github.com/nono/42aee18de6314a621f9126f284e303bb
func (s *Sharing) sendBulkDocs(inst *instance.Instance, m *Member, creds *Credentials, docsByDoctype *DocsByDoctype, ruleIndexes map[string]int) error {
	u, err := url.Parse(m.Instance)
	if err != nil {
		return err
	}
	for doctype, docs := range *docsByDoctype {
		switch doctype {
		case consts.Files:
			s.SortFilesToSent(docs)
			for i, file := range docs {
				fileID := file["_id"].(string)
				s.TransformFileToSent(file, creds.XorKey, ruleIndexes[fileID])
				docs[i] = file
			}
		case consts.BitwardenCiphers:
			for i, doc := range docs {
				s.transformCipherToSent(doc, creds.XorKey)
				docs[i] = doc
			}
		default:
			for i, doc := range docs {
				id := doc["_id"].(string)
				doc["_id"] = XorID(id, creds.XorKey)
				docs[i] = doc
			}
		}
		(*docsByDoctype)[doctype] = docs
	}
	body, err := json.Marshal(docsByDoctype)
	if err != nil {
		return err
	}
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/_bulk_docs",
		Headers: request.Headers{
			echo.HeaderAccept:        echo.MIMEApplicationJSON,
			echo.HeaderContentType:   echo.MIMEApplicationJSON,
			echo.HeaderAuthorization: "Bearer " + creds.AccessToken.AccessToken,
		},
		Body:       bytes.NewReader(body),
		ParseError: ParseRequestError,
		Client:     safehttp.ClientWithKeepAlive,
	}
	res, err := request.Req(opts)
	if res != nil && ShouldRefreshToken(res.StatusCode) {
		res, err = RefreshToken(inst, res, err, s, m, creds, opts, body)
	}
	if err != nil {
		if res != nil && res.StatusCode/100 == 5 {
			return ErrInternalServerError
		}
		return err
	}
	res.Body.Close()
	return nil
}

// ApplyBulkDocs is a multi-doctypes version of the POST _bulk_docs endpoint of CouchDB
func (s *Sharing) ApplyBulkDocs(inst *instance.Instance, payload DocsByDoctype) error {
	mu := config.Lock().ReadWrite(inst, "sharings/"+s.SID+"/_bulk_docs")
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	var refs []*SharedRef

	for doctype, docs := range payload {
		inst.Logger().WithNamespace("replicator").
			Debugf("Apply bulk docs %s: %#v", doctype, docs)
		if doctype == consts.Files {
			err := s.ApplyBulkFiles(inst, docs)
			if err != nil {
				return err
			}
			continue
		}
		var okDocs, docsToUpdate DocsList
		var newRefs, existingRefs []*SharedRef
		newDocs, existingDocs, err := partitionDocsPayload(inst, doctype, docs)
		if err == nil {
			okDocs, newRefs = s.filterDocsToAdd(inst, doctype, newDocs)
			docsToUpdate, existingRefs, err = s.filterDocsToUpdate(inst, doctype, existingDocs)
			if err != nil {
				return err
			}
			okDocs = append(okDocs, docsToUpdate...)
		} else {
			okDocs, newRefs = s.filterDocsToAdd(inst, doctype, docs)
			if len(okDocs) > 0 {
				if err = couchdb.CreateDB(inst, doctype); err != nil {
					return err
				}
			}
		}
		if len(okDocs) > 0 {
			if err = couchdb.BulkForceUpdateDocs(inst, doctype, okDocs); err != nil {
				return err
			}
			for _, doc := range okDocs {
				d := couchdb.JSONDoc{M: doc, Type: doctype}
				event := realtime.EventUpdate
				if doc["_deleted"] != nil {
					event = realtime.EventDelete
				}
				couchdb.RTEvent(inst, event, &d, nil)
			}
			refs = append(refs, newRefs...)
			refs = append(refs, existingRefs...)
		}

		// XXX the bitwarden clients synchronize the ciphers only if the
		// revision date from GET /bitwarden/api/accounts/revision-date has
		// changed. So, we update it here!
		if doctype == consts.BitwardenCiphers {
			if setting, err := settings.Get(inst); err == nil {
				_ = settings.UpdateRevisionDate(inst, setting)
			}
		}
	}

	refsToUpdate := make([]interface{}, len(refs))
	for i, ref := range refs {
		refsToUpdate[i] = ref
	}
	olds := make([]interface{}, len(refsToUpdate))
	return couchdb.BulkUpdateDocs(inst, consts.Shared, refsToUpdate, olds)
}

// partitionDocsPayload returns two slices: the first with documents that are new,
// the second with documents that already exist on this cozy and must be updated.
func partitionDocsPayload(inst *instance.Instance, doctype string, docs DocsList) (news DocsList, existings DocsList, err error) {
	ids := make([]string, len(docs))
	for i, doc := range docs {
		_, ok := doc["_rev"].(string)
		if !ok {
			return nil, nil, ErrMissingRev
		}
		ids[i], ok = doc["_id"].(string)
		if !ok {
			return nil, nil, ErrMissingID
		}
	}
	results := make([]interface{}, 0, len(docs))
	req := couchdb.AllDocsRequest{Keys: ids}
	if err = couchdb.GetAllDocs(inst, doctype, &req, &results); err != nil {
		return nil, nil, err
	}
	for i, doc := range docs {
		if results[i] == nil {
			news = append(news, doc)
		} else {
			existings = append(existings, doc)
		}
	}
	return news, existings, nil
}

// filterDocsToAdd returns a subset of the docs slice with just the documents
// that match a rule of the sharing. It also returns a reference documents to
// put in the io.cozy.shared database.
// https://github.com/golang/go/wiki/SliceTricks#filtering-without-allocating
func (s *Sharing) filterDocsToAdd(inst *instance.Instance, doctype string, docs DocsList) (DocsList, []*SharedRef) {
	filtered := docs[:0]
	refs := make([]*SharedRef, 0, len(docs))
	for _, doc := range docs {
		if _, ok := doc["_deleted"]; ok {
			continue
		}
		r := -1
		for i, rule := range s.Rules {
			if rule.Accept(doctype, doc) {
				r = i
				break
			}
		}
		if r >= 0 {
			ref := SharedRef{
				SID:       doctype + "/" + doc["_id"].(string),
				Revisions: &RevsTree{Rev: doc["_rev"].(string)},
				Infos: map[string]SharedInfo{
					s.SID: {Rule: r},
				},
			}
			refs = append(refs, &ref)
			filtered = append(filtered, doc)
		}
	}
	return filtered, refs
}

// filterDocsToUpdate returns a subset of the docs slice with just the documents
// that are referenced for this sharing in the io.cozy.shared database.
func (s *Sharing) filterDocsToUpdate(inst *instance.Instance, doctype string, docs DocsList) (DocsList, []*SharedRef, error) {
	ids := make([]string, len(docs))
	for i, doc := range docs {
		id, ok := doc["_id"].(string)
		if !ok {
			return nil, nil, ErrMissingID
		}
		ids[i] = doctype + "/" + id
	}
	refs, err := FindReferences(inst, ids)
	if err != nil {
		return nil, nil, err
	}

	filtered := docs[:0]
	frefs := refs[:0]
	for i, doc := range docs {
		if refs[i] != nil {
			infos, ok := refs[i].Infos[s.SID]
			if ok && !infos.Removed {
				rev := doc["_rev"].(string)
				if sub, _ := refs[i].Revisions.Find(rev); sub == nil {
					revs := revsMapToStruct(doc["_revisions"])
					if revs != nil && len(revs.IDs) > 0 {
						chain := revsStructToChain(*revs)
						refs[i].Revisions.InsertChain(chain)
					}
				}
				if _, ok := doc["_deleted"]; ok {
					infos.Removed = true
					refs[i].Infos[s.SID] = infos
				}
				frefs = append(frefs, refs[i])
				filtered = append(filtered, doc)
			}
		}
	}

	return filtered, frefs, nil
}

func (s *Sharing) transformCipherToSent(doc map[string]interface{}, xorKey []byte) {
	id := doc["_id"].(string)
	doc["_id"] = XorID(id, xorKey)
	if orgID, ok := doc["organization_id"].(string); ok {
		doc["organization_id"] = XorID(orgID, xorKey)
	}
}
