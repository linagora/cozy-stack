package jobs

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/justincampbell/bigduration"

	"github.com/cozy/cozy-stack/model/bi"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/emailer"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/web/middlewares"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"

	// import workers
	_ "github.com/cozy/cozy-stack/worker/antivirus"
	_ "github.com/cozy/cozy-stack/worker/archive"
	_ "github.com/cozy/cozy-stack/worker/broker"
	"github.com/cozy/cozy-stack/worker/exec"
	_ "github.com/cozy/cozy-stack/worker/log"
	_ "github.com/cozy/cozy-stack/worker/mails"
	_ "github.com/cozy/cozy-stack/worker/migrations"
	_ "github.com/cozy/cozy-stack/worker/moves"
	_ "github.com/cozy/cozy-stack/worker/notes"
	_ "github.com/cozy/cozy-stack/worker/oauth"
	_ "github.com/cozy/cozy-stack/worker/push"
	_ "github.com/cozy/cozy-stack/worker/rag"
	_ "github.com/cozy/cozy-stack/worker/share"
	_ "github.com/cozy/cozy-stack/worker/sms"
	_ "github.com/cozy/cozy-stack/worker/thumbnail"
	_ "github.com/cozy/cozy-stack/worker/trash"
)

type (
	apiJob struct {
		j *job.Job
	}
	apiJobRequest struct {
		Arguments   json.RawMessage `json:"arguments"`
		Manual      bool            `json:"manual"`
		ForwardLogs bool            `json:"forward_logs"`
		Options     *apiJobOptions  `json:"options"`
	}
	apiJobOptions struct {
		MaxExecCount int `json:"max_exec_count"`
		Timeout      int `json:"timeout"`
	}
	apiSupport struct {
		Arguments map[string]string `json:"arguments"`
	}
	apiCampaign struct {
		Arguments apiCampaignArgs `json:"arguments"`
	}
	apiCampaignArgs struct {
		Subject string      `json:"subject"`
		Parts   []mail.Part `json:"parts"`
	}
	apiQueue struct {
		workerType string
	}
	// apiTrigger is the jsonapi representation for a trigger
	apiTrigger struct {
		t    *job.TriggerInfos
		inst *instance.Instance
	}
	apiTriggerState struct {
		t *job.TriggerInfos
		s *job.TriggerState
	}
	apiTriggerRequest struct {
		Type            string          `json:"type"`
		Arguments       string          `json:"arguments"`
		WorkerType      string          `json:"worker"`
		Message         json.RawMessage `json:"message"`
		WorkerArguments json.RawMessage `json:"worker_arguments"`
		Debounce        string          `json:"debounce"`
		Options         *job.JobOptions `json:"options"`
	}
)

func (j apiJob) ID() string                             { return j.j.ID() }
func (j apiJob) Rev() string                            { return j.j.Rev() }
func (j apiJob) DocType() string                        { return consts.Jobs }
func (j apiJob) Clone() couchdb.Doc                     { return j }
func (j apiJob) SetID(_ string)                         {}
func (j apiJob) SetRev(_ string)                        {}
func (j apiJob) Relationships() jsonapi.RelationshipMap { return nil }
func (j apiJob) Included() []jsonapi.Object             { return nil }
func (j apiJob) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/jobs/" + j.j.WorkerType + "/" + j.j.ID()}
}

func (j apiJob) MarshalJSON() ([]byte, error) {
	return json.Marshal(j.j)
}

func (q apiQueue) ID() string      { return q.workerType }
func (q apiQueue) DocType() string { return consts.Jobs }
func (q apiQueue) Fetch(field string) []string {
	switch field {
	case "worker":
		return []string{q.workerType}
	default:
		return nil
	}
}

// NewAPITrigger creates a jsonapi representation of a trigger.
func NewAPITrigger(infos *job.TriggerInfos, inst *instance.Instance) jsonapi.Object {
	return apiTrigger{infos, inst}
}

func (t apiTrigger) ID() string                             { return t.t.TID }
func (t apiTrigger) Rev() string                            { return "" }
func (t apiTrigger) DocType() string                        { return consts.Triggers }
func (t apiTrigger) Clone() couchdb.Doc                     { return t }
func (t apiTrigger) SetID(_ string)                         {}
func (t apiTrigger) SetRev(_ string)                        {}
func (t apiTrigger) Relationships() jsonapi.RelationshipMap { return nil }
func (t apiTrigger) Included() []jsonapi.Object             { return nil }
func (t apiTrigger) Links() *jsonapi.LinksList {
	links := &jsonapi.LinksList{Self: "/jobs/triggers/" + t.ID()}
	if t.t.Type == "@webhook" {
		links.Webhook = t.inst.PageURL("/jobs/webhooks/"+t.ID(), nil)
	}
	return links
}

func (t apiTrigger) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.t)
}

func (t apiTriggerState) ID() string                             { return t.t.TID }
func (t apiTriggerState) Rev() string                            { return "" }
func (t apiTriggerState) DocType() string                        { return consts.TriggersState }
func (t apiTriggerState) Clone() couchdb.Doc                     { return t }
func (t apiTriggerState) SetID(_ string)                         {}
func (t apiTriggerState) SetRev(_ string)                        {}
func (t apiTriggerState) Relationships() jsonapi.RelationshipMap { return nil }
func (t apiTriggerState) Included() []jsonapi.Object             { return nil }
func (t apiTriggerState) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/jobs/triggers/" + t.ID() + "/state"}
}

func (t apiTriggerState) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.s)
}

const bearerAuthScheme = "Bearer "

// HTTPHandler handle all the `/jobs` routes.
type HTTPHandler struct {
	emailer emailer.Emailer
}

// NewHTTPHandler instantiates a new [HTTPHandler].
func NewHTTPHandler(emailer emailer.Emailer) *HTTPHandler {
	return &HTTPHandler{emailer}
}

func (h *HTTPHandler) getQueue(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	workerType := c.Param("worker-type")

	o := apiQueue{workerType: workerType}
	if err := middlewares.Allow(c, permission.GET, o); err != nil {
		return err
	}

	js, err := job.GetQueuedJobs(instance, workerType)
	if err != nil {
		return wrapJobsError(err)
	}

	objs := make([]jsonapi.Object, len(js))
	for i, j := range js {
		objs[i] = apiJob{j}
	}

	return jsonapi.DataList(c, http.StatusOK, objs, nil)
}

func (h *HTTPHandler) pushJob(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	req := apiJobRequest{}
	if _, err := jsonapi.Bind(c.Request().Body, &req); err != nil {
		return wrapJobsError(err)
	}
	var opts *job.JobOptions
	if req.Options != nil {
		opts = &job.JobOptions{
			MaxExecCount: req.Options.MaxExecCount,
			Timeout:      time.Duration(req.Options.Timeout) * time.Second,
		}
	}

	jr := &job.JobRequest{
		WorkerType:  c.Param("worker-type"),
		Options:     opts,
		Manual:      req.Manual,
		ForwardLogs: req.ForwardLogs,
		Message:     job.Message(req.Arguments),
	}

	if err := middlewares.Allow(c, permission.POST, jr); err != nil {
		return err
	}

	permd, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	if permd.Type != permission.TypeCLI {
		if jr.ForwardLogs {
			return echo.NewHTTPError(http.StatusForbidden)
		}
		if err := checkReservedWorker(jr.WorkerType); err != nil {
			return err
		}
	}

	j, err := job.System().PushJob(instance, jr)
	if err != nil {
		return wrapJobsError(err)
	}

	return jsonapi.Data(c, http.StatusAccepted, apiJob{j}, nil)
}

func (h *HTTPHandler) contactSupport(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	req := apiSupport{}
	if _, err := jsonapi.Bind(c.Request().Body, &req); err != nil {
		return wrapJobsError(err)
	}

	name, _ := settings.PublicName(inst)
	msg, err := job.NewMessage(mail.Options{
		Mode:         mail.ModeSupport,
		TemplateName: "support_request",
		TemplateValues: map[string]interface{}{
			"Name": name,
			"Body": req.Arguments["body"],
		},
		Subject: req.Arguments["subject"],
		Layout:  mail.CozyCloudLayout,
	})
	if err != nil {
		return err
	}
	jr := &job.JobRequest{WorkerType: "sendmail", Message: msg}

	if err := middlewares.AllowWholeType(c, permission.POST, consts.Support); err != nil {
		if middlewares.Allow(c, permission.POST, jr) != nil {
			return err
		}
	}

	if _, err = job.System().PushJob(inst, jr); err != nil {
		return wrapJobsError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) sendCampaignEmail(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	if err := middlewares.Allow(c, permission.POST, &job.JobRequest{WorkerType: "sendmail"}); err != nil {
		return err
	}

	req := apiCampaign{}
	if _, err := jsonapi.Bind(c.Request().Body, &req); err != nil {
		return wrapJobsError(err)
	}

	err := h.emailer.SendCampaignEmail(inst, &emailer.CampaignEmailCmd{
		Subject: req.Arguments.Subject,
		Parts:   req.Arguments.Parts,
	})
	if err != nil {
		return wrapJobsError(err)
	}

	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) newTrigger(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	sched := job.System()
	req := apiTriggerRequest{}
	if _, err := jsonapi.Bind(c.Request().Body, &req); err != nil {
		return wrapJobsError(err)
	}

	if req.Debounce != "" {
		if _, err := time.ParseDuration(req.Debounce); err != nil {
			return jsonapi.InvalidAttribute("debounce", err)
		}
	}

	// Handle metadata
	md := metadata.New()
	if claims := c.Get("claims"); claims != nil {
		cl := claims.(permission.Claims)
		if cl.Subject != "" {
			md.CreatedByApp = cl.Subject
		}
	}
	md.DocTypeVersion = job.DocTypeVersionTrigger

	msg := req.Message
	if req.Message == nil || len(req.Message) == 0 {
		msg = req.WorkerArguments
	}
	t, err := job.NewTrigger(instance, job.TriggerInfos{
		Type:       req.Type,
		WorkerType: req.WorkerType,
		Domain:     instance.Domain,
		Arguments:  req.Arguments,
		Debounce:   req.Debounce,
		Options:    req.Options,
		Metadata:   md,
	}, msg)
	if err != nil {
		return wrapJobsError(err)
	}
	if err = middlewares.Allow(c, permission.POST, t); err != nil {
		return err
	}
	permd, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	if permd.Type != permission.TypeCLI {
		if err := checkReservedWorker(req.WorkerType); err != nil {
			return err
		}
	}

	if err = sched.AddTrigger(t); err != nil {
		return wrapJobsError(err)
	}
	return jsonapi.Data(c, http.StatusCreated, apiTrigger{t.Infos(), instance}, nil)
}

func (h *HTTPHandler) getTrigger(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	sched := job.System()
	t, err := sched.GetTrigger(instance, c.Param("trigger-id"))
	if err != nil {
		return wrapJobsError(err)
	}
	infos := t.Infos()
	if err = middlewares.Allow(c, permission.GET, t); err != nil {
		if !allowKonnectorForItsOwnTrigger(c, infos) {
			return err
		}
	}
	infos.CurrentState, err = job.GetTriggerState(t, t.ID())
	if err != nil {
		return wrapJobsError(err)
	}
	return jsonapi.Data(c, http.StatusOK, apiTrigger{infos, instance}, nil)
}

func (h *HTTPHandler) getTriggerState(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	sched := job.System()
	t, err := sched.GetTrigger(instance, c.Param("trigger-id"))
	if err != nil {
		return wrapJobsError(err)
	}
	infos := t.Infos()
	if err = middlewares.Allow(c, permission.GET, t); err != nil {
		if !allowKonnectorForItsOwnTrigger(c, infos) {
			return err
		}
	}

	state, err := job.GetTriggerState(t, t.ID())
	if err != nil {
		return wrapJobsError(err)
	}
	return jsonapi.Data(c, http.StatusOK, apiTriggerState{t: infos, s: state}, nil)
}

func (h *HTTPHandler) getTriggerJobs(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	var err error

	var limit int
	if queryLimit := c.QueryParam("Limit"); queryLimit != "" {
		limit, err = strconv.Atoi(queryLimit)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err)
		}
	}

	sched := job.System()
	t, err := sched.GetTrigger(instance, c.Param("trigger-id"))
	if err != nil {
		return wrapJobsError(err)
	}
	if err = middlewares.Allow(c, permission.GET, t); err != nil {
		return err
	}

	js, err := job.GetJobs(t, t.ID(), limit)
	if err != nil {
		return wrapJobsError(err)
	}

	objs := make([]jsonapi.Object, len(js))
	for i, j := range js {
		objs[i] = apiJob{j}
	}

	return jsonapi.DataList(c, http.StatusOK, objs, nil)
}

func (h *HTTPHandler) patchTrigger(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	sched := job.System()
	t, err := sched.GetTrigger(inst, c.Param("trigger-id"))
	if err != nil {
		return wrapJobsError(err)
	}
	infos := t.Infos()
	if err := middlewares.Allow(c, permission.PATCH, t); err != nil {
		if !allowKonnectorForItsOwnTrigger(c, infos) {
			return err
		}
	}

	req := apiTriggerRequest{}
	if _, err := jsonapi.Bind(c.Request().Body, &req); err != nil {
		return wrapJobsError(err)
	}
	if req.Arguments == "" && len(req.Message) == 0 {
		return jsonapi.BadRequest(errors.New("Only arguments and message can be patched"))
	}

	if len(req.Message) > 0 {
		if err := sched.UpdateMessage(inst, t, req.Message); err != nil {
			return wrapJobsError(err)
		}
	}

	if req.Arguments != "" {
		if err := sched.UpdateCron(inst, t, req.Arguments); err != nil {
			return wrapJobsError(err)
		}
	}

	return jsonapi.Data(c, http.StatusOK, apiTrigger{infos, inst}, nil)
}

func (h *HTTPHandler) launchTrigger(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	t, err := job.System().GetTrigger(instance, c.Param("trigger-id"))
	if err != nil {
		return wrapJobsError(err)
	}
	if err = middlewares.Allow(c, permission.POST, t); err != nil {
		return err
	}
	req := t.Infos().JobRequest()
	req.Manual = true
	j, err := job.System().PushJob(instance, req)
	if err != nil {
		return wrapJobsError(err)
	}
	if j.WorkerType == "client" {
		if err := j.AckConsumed(); err != nil {
			return wrapJobsError(err)
		}
	}
	return jsonapi.Data(c, http.StatusCreated, apiJob{j}, nil)
}

func (h *HTTPHandler) deleteTrigger(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	sched := job.System()
	t, err := sched.GetTrigger(instance, c.Param("trigger-id"))
	if err != nil {
		return wrapJobsError(err)
	}
	infos := t.Infos()
	if err := middlewares.Allow(c, permission.DELETE, t); err != nil {
		if !allowKonnectorForItsOwnTrigger(c, infos) {
			return err
		}
	}
	if err := sched.DeleteTrigger(instance, c.Param("trigger-id")); err != nil {
		return wrapJobsError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) fireBIWebhook(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	err := config.GetRateLimiter().CheckRateLimit(inst, limits.WebhookTriggerType)
	if limits.IsLimitReachedOrExceeded(err) {
		return echo.NewHTTPError(http.StatusNotFound, "Not found")
	}

	header := c.Request().Header.Get(echo.HeaderAuthorization)
	if !strings.HasPrefix(header, bearerAuthScheme) {
		return middlewares.ErrForbidden
	}
	token := strings.TrimPrefix(header, bearerAuthScheme)

	var payload map[string]interface{}
	if err := c.Bind(&payload); err != nil {
		return jsonapi.BadRequest(err)
	}

	biEvent, err := bi.ParseEventBI(c.QueryParam("event"))
	if err != nil {
		return jsonapi.BadRequest(err)
	}

	// The stack will create or delete accounts and triggers on some webhooks,
	// so it is safer to avoid concurrency on this part of the code.
	mutex := config.Lock().ReadWrite(inst, "bi")
	if err := mutex.Lock(); err != nil {
		return err
	}
	defer mutex.Unlock()

	call := &bi.WebhookCall{
		Instance: inst,
		Token:    token,
		BIurl:    c.QueryParam("bi_url"),
		Event:    biEvent,
		Payload:  payload,
	}
	if err := call.Fire(); err != nil {
		return jsonapi.BadRequest(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) fireWebhook(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	err := config.GetRateLimiter().CheckRateLimit(inst, limits.WebhookTriggerType)
	if limits.IsLimitReachedOrExceeded(err) {
		return echo.NewHTTPError(http.StatusNotFound, "Not found")
	}

	t, err := job.System().GetTrigger(inst, c.Param("trigger-id"))
	if err != nil {
		return wrapJobsError(err)
	}
	webhook, ok := t.(*job.WebhookTrigger)
	if !ok {
		return jsonapi.InvalidAttribute("Type", errors.New("Not a webhook"))
	}

	payload, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return wrapJobsError(err)
	}

	manual := false
	if c.QueryParam("Manual") == "true" {
		manual = true
	}
	webhook.Fire(payload, manual)
	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) getAllTriggers(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	var workerTypes, triggerTypes []string
	if str := c.QueryParam("Worker"); str != "" {
		workerTypes = strings.Split(str, ",")
	}
	if str := c.QueryParam("Type"); str != "" {
		triggerTypes = strings.Split(str, ",")
	}

	if err := middlewares.AllowWholeType(c, permission.GET, consts.Triggers); err != nil {
		if len(workerTypes) != 1 {
			return err
		}
		o := &job.TriggerInfos{WorkerType: workerTypes[0]}
		if err := middlewares.AllowOnFields(c, permission.GET, o, "worker"); err != nil {
			return err
		}
	}

	sched := job.System()
	ts, err := sched.GetAllTriggers(instance)
	if err != nil {
		return wrapJobsError(err)
	}

	objs := make([]jsonapi.Object, 0, len(ts))
	for _, t := range ts {
		tInfos := t.Infos()
		if hasWorker(tInfos, workerTypes) && hasType(tInfos, triggerTypes) {
			tInfos.CurrentState, err = job.GetTriggerState(t, t.ID())
			if err != nil {
				return wrapJobsError(err)
			}
			objs = append(objs, apiTrigger{tInfos, instance})
		}
	}

	return jsonapi.DataList(c, http.StatusOK, objs, nil)
}

func hasWorker(infos *job.TriggerInfos, workers []string) bool {
	if len(workers) == 0 {
		return true
	}
	for _, w := range workers {
		if infos.WorkerType == w {
			return true
		}
	}
	return false
}

func hasType(infos *job.TriggerInfos, triggerTypes []string) bool {
	if len(triggerTypes) == 0 {
		return true
	}
	for _, typ := range triggerTypes {
		if infos.Type == typ {
			return true
		}
	}
	return false
}

func (h *HTTPHandler) getJob(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	j, err := job.Get(instance, c.Param("job-id"))
	if err != nil {
		return err
	}
	if err := middlewares.Allow(c, permission.GET, j); err != nil {
		return err
	}
	return jsonapi.Data(c, http.StatusOK, apiJob{j}, nil)
}

func (h *HTTPHandler) patchJob(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	j, err := job.Get(inst, c.Param("job-id"))
	if err != nil {
		return err
	}
	if err := middlewares.Allow(c, permission.PATCH, j); err != nil {
		return err
	}
	if j.WorkerType != "client" {
		return middlewares.ErrForbidden
	}

	req := job.Job{}
	if _, err := jsonapi.Bind(c.Request().Body, &req); err != nil {
		return wrapJobsError(err)
	}

	log := inst.Logger().
		WithNamespace("jobs").
		WithField("job_id", j.ID()).
		WithField("worker_id", "client")

	msg := &exec.KonnectorMessage{}

	if err := j.Message.Unmarshal(&msg); err == nil {
		log = log.
			WithField("slug", msg.Konnector).
			WithField("account_id", msg.Account).
			WithField("exec_time", time.Since(j.StartedAt))
	}

	switch req.State {
	case job.Errored:
		err = j.Nack(req.Error)
		log.Infof("Konnector failure: %s", req.Error)
		log.Errorf("error while performing job: %s", req.Error)
	case job.Done:
		err = j.Ack()
		log.Info("Konnector success")
	default:
		err = jsonapi.InvalidAttribute("State", errors.New("State must be done or errored"))
	}
	if err != nil {
		return wrapJobsError(err)
	}

	return jsonapi.Data(c, http.StatusOK, apiJob{j}, nil)
}

func (h *HTTPHandler) cleanJobs(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.POST, consts.Jobs); err != nil {
		return err
	}
	var ups []*job.Job
	now := time.Now()
	err := couchdb.ForeachDocs(instance, consts.Jobs, func(_ string, data json.RawMessage) error {
		var j *job.Job
		if err := json.Unmarshal(data, &j); err != nil {
			return err
		}
		if j.State == job.Running || j.State == job.Queued {
			if j.StartedAt.Add(1 * time.Hour).Before(now) {
				ups = append(ups, j)
			}
		}
		return nil
	})
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return err
	}
	var errf error
	for _, j := range ups {
		j.State = job.Done
		err := couchdb.UpdateDoc(instance, j)
		if err != nil {
			errf = multierror.Append(errf, err)
		}
	}
	if errf != nil {
		return errf
	}
	return c.JSON(200, map[string]int{"deleted": len(ups)})
}

func (h *HTTPHandler) purgeJobs(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.DELETE, consts.Jobs); err != nil {
		return err
	}

	workersParam := c.QueryParam("workers")
	durationParam := c.QueryParam("duration")

	conf := config.GetConfig().Jobs
	dur, err := bigduration.ParseDuration(conf.DefaultDurationToKeep)
	if err != nil {
		return err
	}

	if durationParam != "" {
		dur, err = bigduration.ParseDuration(durationParam)
		if err != nil {
			return err
		}
	}
	workers := job.GetWorkersNamesList()
	if workersParam != "" {
		workers = strings.Split(workersParam, ",")
	}

	allJobs, err := job.GetAllJobs(instance)
	if err != nil {
		return err
	}

	// Step 1: We want to get all the jobs prior to the date parameter.
	// Jobs returned are the ones we want to remove
	d := time.Now().Add(-dur)
	jobsBeforeDate := job.FilterJobsBeforeDate(allJobs, d)

	// Step 2: We also want to keep a minimum number of jobs for each state.
	// Jobs returned will be kept.
	lastsJobs := map[string]struct{}{}
	for _, w := range workers {
		jobs, err := job.GetLastsJobs(allJobs, w)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			lastsJobs[j.ID()] = struct{}{}
		}
	}

	// Step 3: cleaning.
	// - Removing jobs from the ids if they exists.
	// - Skipping worker types
	var finalJobs []*job.Job

	for _, j := range jobsBeforeDate {
		validWorker := false

		for _, wt := range workers {
			if j.WorkerType == wt {
				validWorker = true
				break
			}
		}
		// Check the job is not existing in the lasts jobs
		if validWorker {
			_, ok := lastsJobs[j.ID()]

			if !ok {
				finalJobs = append(finalJobs, j)
			}
		}
	}

	// Bulk-deleting the jobs
	jobsToDelete := make([]couchdb.Doc, len(finalJobs))
	for i, j := range finalJobs {
		jobsToDelete[i] = j
	}

	chunkSize := 1000

	for i := 0; i < len(jobsToDelete); i += chunkSize {
		end := i + chunkSize

		if end > len(jobsToDelete) {
			end = len(jobsToDelete)
		}

		err = couchdb.BulkDeleteDocs(instance, consts.Jobs, jobsToDelete[i:end])
		if err != nil {
			return err
		}
	}

	return c.JSON(http.StatusOK, map[string]int{"deleted": len(jobsToDelete)})
}

// Register all the `/jobs` routes to the given router
func (h *HTTPHandler) Register(router *echo.Group) {
	router.GET("/queue/:worker-type", h.getQueue)
	router.POST("/queue/:worker-type", h.pushJob)
	router.POST("/support", h.contactSupport)
	router.POST("/campaign-emails", h.sendCampaignEmail)

	router.POST("/triggers", h.newTrigger)
	router.GET("/triggers", h.getAllTriggers)
	router.GET("/triggers/:trigger-id", h.getTrigger)
	router.GET("/triggers/:trigger-id/state", h.getTriggerState)
	router.GET("/triggers/:trigger-id/jobs", h.getTriggerJobs)
	router.PATCH("/triggers/:trigger-id", h.patchTrigger)
	router.POST("/triggers/:trigger-id/launch", h.launchTrigger)
	router.DELETE("/triggers/:trigger-id", h.deleteTrigger)

	router.POST("/webhooks/bi", h.fireBIWebhook)
	router.POST("/webhooks/:trigger-id", h.fireWebhook)

	router.POST("/clean", h.cleanJobs)
	router.DELETE("/purge", h.purgeJobs)
	router.GET("/:job-id", h.getJob)
	router.PATCH("/:job-id", h.patchJob)
}

func wrapJobsError(err error) error {
	switch err {
	case job.ErrNotFoundTrigger,
		job.ErrNotFoundJob,
		job.ErrUnknownWorker:
		return jsonapi.NotFound(err)
	case job.ErrUnknownTrigger,
		job.ErrNotCronTrigger:
		return jsonapi.InvalidAttribute("Type", err)
	case emailer.ErrMissingSubject,
		emailer.ErrMissingContent,
		limits.ErrRateLimitReached,
		limits.ErrRateLimitExceeded:
		return jsonapi.BadRequest(err)
	}
	return err
}

// checkReservedWorker returns an error if the worker should only by used by
// the stack, and the clients must not push jobs for it.
func checkReservedWorker(worker string) error {
	reserved, err := job.System().WorkerIsReserved(worker)
	if err != nil {
		if errors.Is(err, job.ErrUnknownWorker) {
			return echo.NewHTTPError(http.StatusNotFound)
		}
		return err
	}
	if reserved {
		return echo.NewHTTPError(http.StatusForbidden)
	}
	return nil
}

func allowKonnectorForItsOwnTrigger(c echo.Context, infos *job.TriggerInfos) bool {
	if infos.WorkerType != "konnector" {
		return false
	}
	var msg map[string]interface{}
	if errb := json.Unmarshal(infos.Message, &msg); errb != nil {
		return false
	}
	slug, _ := msg["konnector"].(string)
	if slug == "" {
		return false
	}
	err := middlewares.AllowForKonnector(c, slug)
	return err == nil
}
