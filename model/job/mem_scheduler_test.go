package job_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemScheduler(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	testInstance := setup.GetTestInstance()

	t.Run("TriggersBadArguments", func(t *testing.T) {
		var err error
		_, err = job.NewTrigger(testInstance, job.TriggerInfos{
			Type:      "@at",
			Arguments: "garbage",
		}, nil)
		assert.Error(t, err)

		_, err = job.NewTrigger(testInstance, job.TriggerInfos{
			Type:      "@in",
			Arguments: "garbage",
		}, nil)
		assert.Error(t, err)

		_, err = job.NewTrigger(testInstance, job.TriggerInfos{
			Type:      "@unknown",
			Arguments: "",
		}, nil)
		if assert.Error(t, err) {
			assert.Equal(t, job.ErrUnknownTrigger, err)
		}
	})

	t.Run("MemSchedulerWithDebounce", func(t *testing.T) {
		var called int32
		bro := job.NewMemBroker()
		assert.NoError(t, bro.StartWorkers(job.WorkersList{
			{
				WorkerType:   "worker",
				Concurrency:  1,
				MaxExecCount: 1,
				Timeout:      1 * time.Millisecond,
				WorkerFunc: func(_ *job.TaskContext) error {
					atomic.AddInt32(&called, 1)
					return nil
				},
			},
		}))

		msg, _ := job.NewMessage("@event")
		ti := job.TriggerInfos{
			Type:       "@event",
			Arguments:  "io.cozy.testdebounce io.cozy.moredebounce",
			Debounce:   "2s",
			WorkerType: "worker",
			Message:    msg,
		}

		var triggers []job.Trigger
		triggersInfos := []job.TriggerInfos{ti}
		sch := job.NewMemScheduler()
		if !assert.NoError(t, sch.StartScheduler(bro)) {
			return
		}

		// Clear the existing triggers before testing with our triggers
		ts, err := sch.GetAllTriggers(testInstance)
		assert.NoError(t, err)
		for _, trigger := range ts {
			err = sch.DeleteTrigger(testInstance, trigger.ID())
			assert.NoError(t, err)
		}

		for _, infos := range triggersInfos {
			trigger, err := job.NewTrigger(testInstance, infos, msg)
			require.NoError(t, err)

			err = sch.AddTrigger(trigger)
			require.NoError(t, err)

			triggers = append(triggers, trigger)
		}

		ts, err = sch.GetAllTriggers(testInstance)
		assert.NoError(t, err)
		assert.Len(t, ts, len(triggers))

		doc := &couchdb.JSONDoc{
			Type: "io.cozy.testdebounce",
			M: map[string]interface{}{
				"_id":  "test-id",
				"_rev": "1-xxabxx",
				"test": "value",
			},
		}

		for i := 0; i < 24; i++ {
			time.Sleep(200 * time.Millisecond)
			realtime.GetHub().Publish(testInstance, realtime.EventCreate, doc, nil)
		}

		time.Sleep(3000 * time.Millisecond)
		assert.Equal(t, int32(3), atomic.LoadInt32(&called))

		doc2 := doc.Clone().(*couchdb.JSONDoc)
		doc2.Type = "io.cozy.moredebounce"
		realtime.GetHub().Publish(testInstance, realtime.EventCreate, doc, nil)
		realtime.GetHub().Publish(testInstance, realtime.EventCreate, doc2, nil)
		time.Sleep(3000 * time.Millisecond)
		assert.Equal(t, int32(4), atomic.LoadInt32(&called))

		for _, trigger := range triggers {
			err = sch.DeleteTrigger(testInstance, trigger.ID())
			assert.NoError(t, err)
		}

		err = sch.ShutdownScheduler(context.Background())
		assert.NoError(t, err)
	})

	t.Run("OneShotTriggerIsDeletedOnceFired", func(t *testing.T) {
		var called int32
		bro := job.NewMemBroker()
		require.NoError(t, bro.StartWorkers(job.WorkersList{{
			WorkerType:   "one-shot",
			Concurrency:  1,
			MaxExecCount: 1,
			Timeout:      time.Second,
			WorkerFunc: func(_ *job.TaskContext) error {
				atomic.AddInt32(&called, 1)
				return nil
			},
		}}))
		sch := job.NewMemScheduler()
		require.NoError(t, sch.StartScheduler(bro))
		t.Cleanup(func() { _ = sch.ShutdownScheduler(context.Background()) })

		infos := job.TriggerInfos{Type: "@in", WorkerType: "one-shot", Arguments: "10ms"}
		trigger, err := job.NewTrigger(testInstance, infos, nil)
		require.NoError(t, err)
		require.NoError(t, sch.AddTrigger(trigger))
		require.Eventually(t, func() bool {
			return atomic.LoadInt32(&called) == 1 && !sch.HasTrigger(testInstance, infos)
		}, 5*time.Second, 10*time.Millisecond, "a fired @in trigger no longer counts as pending")
		_, err = sch.GetTrigger(testInstance, trigger.ID())
		require.ErrorIs(t, err, job.ErrNotFoundTrigger)
	})
}
