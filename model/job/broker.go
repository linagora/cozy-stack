package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

const (
	// Queued state
	Queued State = "queued"
	// Running state
	Running State = "running"
	// Done state
	Done State = "done"
	// Errored state
	Errored State = "errored"
)

// defaultMaxLimits defines the maximum limit of how much jobs will be returned
// for each job state
var defaultMaxLimits map[State]int = map[State]int{
	Queued:  50,
	Running: 50,
	Done:    50,
	Errored: 50,
}

type (
	// Broker interface is used to represent a job broker associated to a
	// particular domain. A broker can be used to create jobs that are pushed in
	// the job system.
	//
	// This interface is matched by several implementations:
	// - [BrokerMock] a mock implementation used for the tests.
	Broker interface {
		StartWorkers(workersList WorkersList) error
		ShutdownWorkers(ctx context.Context) error

		// PushJob will push try to push a new job from the specified job request.
		// This method is asynchronous.
		PushJob(db prefixer.Prefixer, request *JobRequest) (*Job, error)

		// WorkerQueueLen returns the total element in the queue of the specified
		// worker type.
		WorkerQueueLen(workerType string) (int, error)
		// WorkerIsReserved returns true if the given worker type is reserved
		// (ie clients should not push jobs to it, only the stack).
		WorkerIsReserved(workerType string) (bool, error)
		// WorkersTypes returns the list of registered workers types.
		WorkersTypes() []string
	}

	// State represent the state of a job.
	State string

	// Message is a json encoded job message.
	Message json.RawMessage

	// Event is a json encoded value of a realtime.Event.
	Event json.RawMessage

	// Payload is a json encode value of a webhook payload.
	Payload json.RawMessage

	// Job contains all the metadata informations of a Job. It can be
	// marshalled in JSON.
	Job struct {
		JobID       string      `json:"_id,omitempty"`
		JobRev      string      `json:"_rev,omitempty"`
		Cluster     int         `json:"couch_cluster,omitempty"`
		Domain      string      `json:"domain"`
		Prefix      string      `json:"prefix,omitempty"`
		WorkerType  string      `json:"worker"`
		TriggerID   string      `json:"trigger_id,omitempty"`
		Message     Message     `json:"message"`
		Event       Event       `json:"event"`
		Payload     Payload     `json:"payload,omitempty"`
		Manual      bool        `json:"manual_execution,omitempty"`
		Debounced   bool        `json:"debounced,omitempty"`
		Options     *JobOptions `json:"options,omitempty"`
		State       State       `json:"state"`
		QueuedAt    time.Time   `json:"queued_at"`
		StartedAt   time.Time   `json:"started_at"`
		FinishedAt  time.Time   `json:"finished_at"`
		Error       string      `json:"error,omitempty"`
		ForwardLogs bool        `json:"forward_logs,omitempty"`
	}

	// JobRequest struct is used to represent a new job request.
	JobRequest struct {
		WorkerType  string
		TriggerID   string
		Trigger     Trigger
		Message     Message
		Event       Event
		Payload     Payload
		Manual      bool
		Debounced   bool
		ForwardLogs bool
		Options     *JobOptions
	}

	// JobOptions struct contains the execution properties of the jobs.
	JobOptions struct {
		MaxExecCount int           `json:"max_exec_count"`
		Timeout      time.Duration `json:"timeout"`
	}
)

var joblog = logger.WithNamespace("jobs")

// DBCluster implements the prefixer.Prefixer interface.
func (j *Job) DBCluster() int {
	return j.Cluster
}

// DBPrefix implements the prefixer.Prefixer interface.
func (j *Job) DBPrefix() string {
	if j.Prefix != "" {
		return j.Prefix
	}
	return j.Domain
}

// DomainName implements the prefixer.Prefixer interface.
func (j *Job) DomainName() string {
	return j.Domain
}

// ID implements the couchdb.Doc interface
func (j *Job) ID() string { return j.JobID }

// Rev implements the couchdb.Doc interface
func (j *Job) Rev() string { return j.JobRev }

// Clone implements the couchdb.Doc interface
func (j *Job) Clone() couchdb.Doc {
	cloned := *j
	if j.Options != nil {
		tmp := *j.Options
		cloned.Options = &tmp
	}
	if j.Message != nil {
		tmp := j.Message
		j.Message = make([]byte, len(tmp))
		copy(j.Message[:], tmp)
	}
	if j.Event != nil {
		tmp := j.Event
		j.Event = make([]byte, len(tmp))
		copy(j.Event[:], tmp)
	}
	if j.Payload != nil {
		tmp := j.Payload
		j.Payload = make([]byte, len(tmp))
		copy(j.Payload[:], tmp)
	}
	return &cloned
}

// DocType implements the couchdb.Doc interface
func (j *Job) DocType() string { return consts.Jobs }

// SetID implements the couchdb.Doc interface
func (j *Job) SetID(id string) { j.JobID = id }

// SetRev implements the couchdb.Doc interface
func (j *Job) SetRev(rev string) { j.JobRev = rev }

// Fetch implements the permission.Fetcher interface
func (j *Job) Fetch(field string) []string {
	switch field {
	case "worker":
		return []string{j.WorkerType}
	case "state":
		return []string{fmt.Sprintf("%v", j.State)}
	}
	return nil
}

// ID implements the permission.Getter interface
func (jr *JobRequest) ID() string { return "" }

// DocType implements the permission.Getter interface
func (jr *JobRequest) DocType() string { return consts.Jobs }

// Fetch implements the permission.Fetcher interface
func (jr *JobRequest) Fetch(field string) []string {
	switch field {
	case "worker":
		return []string{jr.WorkerType}
	default:
		return nil
	}
}

// Logger returns a logger associated with the job domain
func (j *Job) Logger() *logger.Entry {
	return logger.WithDomain(j.Domain).WithNamespace("jobs")
}

// AckConsumed sets the job infos state to Running an sends the new job infos
// on the channel.
func (j *Job) AckConsumed() error {
	j.Logger().Debugf("ack_consume %s", j.ID())
	j.StartedAt = time.Now()
	j.State = Running
	return j.Update()
}

// Ack sets the job infos state to Done an sends the new job infos on the
// channel.
func (j *Job) Ack() error {
	j.Logger().Debugf("ack %s", j.ID())
	j.FinishedAt = time.Now()
	j.State = Done
	j.Event = nil
	j.Payload = nil
	return j.Update()
}

// Nack sets the job infos state to Errored, set the specified error has the
// error field and sends the new job infos on the channel.
func (j *Job) Nack(errorMessage string) error {
	j.Logger().Debugf("nack %s", j.ID())
	j.FinishedAt = time.Now()
	j.State = Errored
	j.Error = errorMessage
	j.Event = nil
	j.Payload = nil
	return j.Update()
}

// Update updates the job in couchdb
func (j *Job) Update() error {
	err := couchdb.UpdateDoc(j, j)
	// XXX When a job for an import runs, the database for io.cozy.jobs is
	// deleted, and we need to recreate the job, not just update it.
	if couchdb.IsNotFoundError(err) {
		j.SetID("")
		j.SetRev("")
		return j.Create()
	}
	return err
}

// Create creates the job in couchdb
func (j *Job) Create() error {
	return j.create(context.Background())
}

func (j *Job) create(ctx context.Context) error {
	return couchdb.CreateDocWithContext(ctx, j, j)
}

// WaitUntilDone will wait until the job is done. It will return an error if
// the job has failed. And there is a timeout (10 minutes).
func (j *Job) WaitUntilDone(db prefixer.Prefixer) error {
	sub := realtime.GetHub().Subscriber(db)
	defer sub.Close()
	sub.Watch(j.DocType(), j.ID())
	timeout := time.After(10 * time.Minute)
	for {
		select {
		case e := <-sub.Channel:
			state := Queued
			if doc, ok := e.Doc.(*couchdb.JSONDoc); ok {
				stateStr, _ := doc.M["state"].(string)
				state = State(stateStr)
			} else if doc, ok := e.Doc.(*realtime.JSONDoc); ok {
				stateStr, _ := doc.M["state"].(string)
				state = State(stateStr)
			} else if doc, ok := e.Doc.(*Job); ok {
				state = doc.State
			}
			switch state {
			case Done:
				return nil
			case Errored:
				return errors.New("The konnector failed on account deletion")
			}
		case <-timeout:
			return nil
		}
	}
}

// UnmarshalJSON implements json.Unmarshaler on Message. It should be retro-
// compatible with the old Message representation { Data, Type }.
func (m *Message) UnmarshalJSON(data []byte) error {
	// For retro-compatibility purposes
	var mm struct {
		Data []byte `json:"Data"`
		Type string `json:"Type"`
	}
	if err := json.Unmarshal(data, &mm); err == nil && mm.Type == "json" {
		var v json.RawMessage
		if err = json.Unmarshal(mm.Data, &v); err != nil {
			return err
		}
		*m = Message(v)
		return nil
	}
	var v json.RawMessage
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*m = Message(v)
	return nil
}

// MarshalJSON implements json.Marshaler on Message.
func (m Message) MarshalJSON() ([]byte, error) {
	v := json.RawMessage(m)
	return json.Marshal(v)
}

// NewJob creates a new Job instance from a job request.
func NewJob(db prefixer.Prefixer, req *JobRequest) *Job {
	return &Job{
		Cluster:     db.DBCluster(),
		Domain:      db.DomainName(),
		Prefix:      db.DBPrefix(),
		WorkerType:  req.WorkerType,
		TriggerID:   req.TriggerID,
		Manual:      req.Manual,
		Message:     req.Message,
		Debounced:   req.Debounced,
		Event:       req.Event,
		Payload:     req.Payload,
		Options:     req.Options,
		ForwardLogs: req.ForwardLogs,
		State:       Queued,
		QueuedAt:    time.Now(),
	}
}

// Get returns the informations about a job.
func Get(db prefixer.Prefixer, jobID string) (*Job, error) {
	var job Job
	if err := couchdb.GetDoc(db, consts.Jobs, jobID, &job); err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil, ErrNotFoundJob
		}
		return nil, err
	}
	return &job, nil
}

// GetQueuedJobs returns the list of jobs which states is "queued" or "running"
func GetQueuedJobs(db prefixer.Prefixer, workerType string) ([]*Job, error) {
	var results []*Job
	req := &couchdb.FindRequest{
		UseIndex: "by-worker-and-state",
		Selector: mango.And(
			mango.Equal("worker", workerType),
			mango.Exists("state"), // XXX it is needed by couchdb to use the index
			mango.Or(
				mango.Equal("state", Queued),
				mango.Equal("state", Running),
			),
		),
		Limit: 200,
	}
	err := couchdb.FindDocs(db, consts.Jobs, req, &results)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// GetAllJobs returns the list of all the jobs on the given instance.
func GetAllJobs(db prefixer.Prefixer) ([]*Job, error) {
	var startkey string
	var lastJob *Job

	finalJobs := []*Job{}
	remainingJobs := true

	for remainingJobs {
		jobs := []*Job{}
		req := &couchdb.AllDocsRequest{
			Limit:    10001,
			StartKey: startkey,
		}

		err := couchdb.GetAllDocs(db, consts.Jobs, req, &jobs)
		if err != nil {
			return nil, err
		}

		if len(jobs) == 0 {
			return finalJobs, nil
		}

		lastJob, jobs = jobs[len(jobs)-1], jobs[:len(jobs)-1]

		// Startkey for the next request
		startkey = lastJob.JobID

		// Appending to the final jobs
		finalJobs = append(finalJobs, jobs...)

		// Only the startkey is present: we are in the last lap of the loop
		// We have to append the startkey as the last element
		if len(jobs) == 0 {
			remainingJobs = false
			finalJobs = append(finalJobs, lastJob)
		}
	}

	return finalJobs, nil
}

// FilterJobsBeforeDate returns alls jobs queued before the specified date
func FilterJobsBeforeDate(jobs []*Job, date time.Time) []*Job {
	b := []*Job{}

	for _, x := range jobs {
		if x.QueuedAt.Before(date) {
			b = append(b, x)
		}
	}

	return b
}

// FilterByWorkerAndState filters a job slice by its workerType and State
func FilterByWorkerAndState(jobs []*Job, workerType string, state State, limit int) []*Job {
	returned := []*Job{}
	for _, j := range jobs {
		if j.WorkerType == workerType && j.State == state {
			returned = append(returned, j)
			if len(returned) == limit {
				return returned
			}
		}
	}

	return returned
}

// GetLastsJobs returns the N lasts job of each state for an instance/worker
// type pair
func GetLastsJobs(jobs []*Job, workerType string) ([]*Job, error) {
	var result []*Job

	// Ordering by QueuedAt before filtering jobs
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].QueuedAt.Before(jobs[j].QueuedAt) })

	for _, state := range []State{Queued, Running, Done, Errored} {
		limit := defaultMaxLimits[state]

		filtered := FilterByWorkerAndState(jobs, workerType, state, limit)
		result = append(result, filtered...)
	}

	return result, nil
}

// NewMessage returns a json encoded data
func NewMessage(data interface{}) (Message, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return Message(b), nil
}

// NewEvent return a json encoded realtime.Event
func NewEvent(data *realtime.Event) (Event, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return Event(b), nil
}

// Unmarshal can be used to unmarshal the encoded message value in the
// specified interface's type.
func (m Message) Unmarshal(msg interface{}) error {
	if m == nil {
		return ErrMessageNil
	}
	if err := json.Unmarshal(m, &msg); err != nil {
		return ErrMessageUnmarshal
	}
	return nil
}

// Unmarshal can be used to unmarshal the encoded message value in the
// specified interface's type.
func (e Event) Unmarshal(evt interface{}) error {
	if e == nil {
		return ErrMessageNil
	}
	if err := json.Unmarshal(e, &evt); err != nil {
		return ErrMessageUnmarshal
	}
	return nil
}

// Unmarshal can be used to unmarshal the encoded message value in the
// specified interface's type.
func (p Payload) Unmarshal(evt interface{}) error {
	if p == nil {
		return ErrMessageNil
	}
	if err := json.Unmarshal(p, &evt); err != nil {
		return ErrMessageUnmarshal
	}
	return nil
}

var (
	_ permission.Fetcher = (*JobRequest)(nil)
	_ permission.Fetcher = (*Job)(nil)
)
