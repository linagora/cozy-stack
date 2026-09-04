package jobs

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/emailer"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	_ "github.com/cozy/cozy-stack/worker/rag"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func setupRouter(t *testing.T, inst *instance.Instance, emailerSvc emailer.Emailer) *httptest.Server {
	t.Helper()

	handler := echo.New()
	handler.HTTPErrorHandler = errors.ErrorHandler
	group := handler.Group("/jobs", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(context echo.Context) error {
			context.Set("instance", inst)

			tok := middlewares.GetRequestToken(context)
			// Forcing the token parsing to have the "claims" parameter in the
			// context (in production, it is done via
			// middlewares.CheckInstanceBlocked)
			_, err := middlewares.ParseJWT(context, inst, tok)
			if err != nil {
				return err
			}

			return next(context)
		}
	})

	NewHTTPHandler(emailerSvc).Register(group)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts
}

func TestJobs(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())

	job.AddWorker(&job.WorkerConfig{
		WorkerType:  "print",
		Concurrency: 4,
		WorkerFunc: func(ctx *job.TaskContext) error {
			var msg string
			if err := ctx.UnmarshalMessage(&msg); err != nil {
				return err
			}

			t.Log(msg)

			return nil
		},
	})

	testInstance := setup.GetTestInstance()

	scope := strings.Join([]string{
		consts.Jobs + ":ALL:print:worker",
		consts.Triggers + ":ALL:print:worker",
	}, " ")
	token, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI", scope,
		"", time.Now())

	emailerSvc := emailer.NewMock(t)
	ts := setupRouter(t, testInstance, emailerSvc)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	t.Run("GetQueue", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/jobs/queue/print").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Value("data").Array().
			Length().IsEqual(0)
	})

	t.Run("CreateJob", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/jobs/queue/print").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "attributes": { "arguments": "foobar" }
        }
      }`)).
			Expect().Status(202).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.HasValue("worker", "print")
		attrs.NotContainsKey("manual_execution")
	})

	t.Run("CreateJobWithTimeout", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/jobs/queue/print").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "attributes": {
		    "arguments": "foobar",
		    "options": { "timeout": 42 }
		  }
        }
      }`)).
			Expect().Status(202).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		jobID := obj.Path("$.data.id").String().NotEmpty().Raw()
		job, err := job.Get(testInstance, jobID)
		require.NoError(t, err)
		require.NotNil(t, job.Options)
		assert.Equal(t, 42*time.Second, job.Options.Timeout)
	})

	t.Run("CreateManualJob", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/jobs/queue/print").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "arguments": "foobar",
            "manual": true
          }
        }
      }`)).
			Expect().Status(202).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.HasValue("worker", "print")
		attrs.HasValue("manual_execution", true)
	})

	t.Run("CreateJobForReservedWorker", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/jobs/queue/trash-files").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{"data": {"attributes": {"arguments": "foobar"}}}`)).
			Expect().Status(403)
	})

	t.Run("CreateRagIndexTriggerWithAppToken", func(t *testing.T) {
		// Only the permission check on the rag-index worker is under test:
		// the trigger arguments and the dir_id are arbitrary.
		_, appToken := setup.GetTestClient(consts.Triggers)
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/jobs/triggers").
			WithHeader("Authorization", "Bearer "+appToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
				"data": {
					"attributes": {
						"type": "@event",
						"arguments": "io.cozy.ai.chat.assistants",
						"debounce": "1m",
						"worker": "rag-index",
						"message": {"doctype": "io.cozy.files", "dir_id": "kb"}
					}
				}
			}`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()
		triggerID := obj.Path("$.data.id").String().NotEmpty().Raw()

		e.DELETE("/jobs/triggers/"+triggerID).
			WithHeader("Authorization", "Bearer "+appToken).
			Expect().Status(204)

		// rag-query stays reserved.
		e.POST("/jobs/queue/rag-query").
			WithHeader("Authorization", "Bearer "+appToken).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{"data": {"attributes": {"arguments": {}}}}`)).
			Expect().Status(403)
	})

	t.Run("CreateJobNotExist", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		tokenNone, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI",
			consts.Jobs+":ALL:none:worker",
			"", time.Now())

		e.POST("/jobs/queue/none"). // invalid
						WithHeader("Authorization", "Bearer "+tokenNone).
						WithHeader("Content-Type", "application/json").
						WithBytes([]byte(`{"data": {"attributes": {"arguments": "foobar"}}}`)).
						Expect().Status(404)
	})

	t.Run("AddGetAndDeleteTriggerAt", func(t *testing.T) {
		var triggerID string
		at := time.Now().Add(1100 * time.Millisecond).Format(time.RFC3339)

		t.Run("AddSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@at",
            "arguments": "` + at + `",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.HasValue("type", consts.Triggers)

			attrs := data.Value("attributes").Object()
			attrs.HasValue("arguments", at)
			attrs.HasValue("worker", "print")
		})

		t.Run("AddFailure", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@at",
            "arguments": "garbage",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(400)
		})

		t.Run("GetSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(200)
		})

		t.Run("DeleteSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.DELETE("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)
		})

		t.Run("GetNotFound", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(404)
		})
	})

	t.Run("AddGetAndDeleteTriggerIn", func(t *testing.T) {
		var triggerID string

		t.Run("AddSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@in",
            "arguments": "1s",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.HasValue("type", consts.Triggers)

			attrs := data.Value("attributes").Object()
			attrs.HasValue("type", "@in")
			attrs.HasValue("arguments", "1s")
			attrs.HasValue("worker", "print")
		})

		t.Run("AddFailure", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@in",
            "arguments": "garbage",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(400)
		})

		t.Run("GetSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(200)
		})

		t.Run("DeleteSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.DELETE("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)
		})

		t.Run("GetNotFound", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(404)
		})
	})

	t.Run("AddGetUpdateAndDeleteTriggerCron", func(t *testing.T) {
		var triggerID string

		t.Run("AddSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@cron",
            "arguments": "0 0 0 * * 0",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.HasValue("type", consts.Triggers)

			attrs := data.Value("attributes").Object()
			attrs.HasValue("type", "@cron")
			attrs.HasValue("arguments", "0 0 0 * * 0")
			attrs.HasValue("worker", "print")
		})

		t.Run("PatchArgumentsSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.PATCH("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "arguments": "0 0 0 * * 1"
          }
        }
      }`)).
				Expect().Status(200)
		})

		t.Run("PatchMessageSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.PATCH("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
			"message": {
			  "folder_to_save": "123"
			}
          }
        }
      }`)).
				Expect().Status(200)
		})

		t.Run("GetSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.HasValue("type", consts.Triggers)

			attrs := data.Value("attributes").Object()
			attrs.HasValue("type", "@cron")
			attrs.HasValue("arguments", "0 0 0 * * 1")
			attrs.HasValue("worker", "print")
		})

		t.Run("DeleteSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.DELETE("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)
		})
	})

	t.Run("AddTriggerWithMetadata", func(t *testing.T) {
		var triggerID string

		at := time.Now().Add(1100 * time.Millisecond).Format(time.RFC3339)

		t.Run("AddSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@webhook",
            "arguments": "` + at + `",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.HasValue("type", consts.Triggers)
			data.Path("$.links.webhook").IsEqual("https://" + testInstance.Domain + "/jobs/webhooks/" + triggerID)

			attrs := data.Value("attributes").Object()
			attrs.HasValue("type", "@webhook")
			attrs.HasValue("arguments", at)
			attrs.HasValue("worker", "print")

			metas := attrs.Value("cozyMetadata").Object()
			metas.HasValue("doctypeVersion", "1")
			metas.HasValue("metadataVersion", 1)
			metas.HasValue("createdByApp", "CLI")
			metas.Value("createdAt").String().AsDateTime(time.RFC3339)
			metas.Value("updatedAt").String().AsDateTime(time.RFC3339)
		})

		t.Run("GetSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(200)
		})

		t.Run("DeleteSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.DELETE("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)
		})
	})

	t.Run("GetAllJobs", func(t *testing.T) {
		tokenTriggers, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI", consts.Triggers, "", time.Now())

		t.Run("GetNoJobs", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().
				Value("data").Array().IsEmpty()
		})

		t.Run("CreateAJob", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				WithHeader("Content-Type", "application/json").
				// worker_arguments is deprecated but should still works
				// we are using it here to check that it still works
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@in",
            "arguments": "10s",
            "worker": "print",
            "worker_arguments": "foo"
          }
        }
      }`)).
				Expect().Status(201)
		})

		t.Run("GetAllJobs", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Value("data").Array().Length().IsEqual(1)
			elem := obj.Value("data").Array().Value(0).Object()
			elem.HasValue("type", consts.Triggers)
			attrs := elem.Value("attributes").Object()
			attrs.HasValue("type", "@in")
			attrs.HasValue("arguments", "10s")
			attrs.HasValue("worker", "print")
		})

		t.Run("WithWorkerQueryAndResult", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/jobs/triggers").
				WithQuery("Worker", "print").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Value("data").Array().Length().IsEqual(1)
			elem := obj.Value("data").Array().Value(0).Object()
			elem.HasValue("type", consts.Triggers)
			attrs := elem.Value("attributes").Object()
			attrs.HasValue("type", "@in")
			attrs.HasValue("arguments", "10s")
			attrs.HasValue("worker", "print")
		})

		t.Run("WithWorkerQueryAndNoResults", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers").
				WithQuery("Worker", "nojobforme"). // no matching job
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().Value("data").
				Array().IsEmpty()
		})

		t.Run("WithTypeQuery", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/jobs/triggers").
				WithQuery("Type", "@in").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Value("data").Array().Length().IsEqual(1)
			elem := obj.Value("data").Array().Value(0).Object()
			elem.HasValue("type", consts.Triggers)
			attrs := elem.Value("attributes").Object()
			attrs.HasValue("type", "@in")
			attrs.HasValue("arguments", "10s")
			attrs.HasValue("worker", "print")
		})
	})

	t.Run("ClientJobs", func(t *testing.T) {
		var triggerID string
		var jobID string

		scope := consts.Jobs + " " + consts.Triggers
		token, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI", scope, "", time.Now())

		t.Run("CreateAClientJob", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
	       "data": {
	         "attributes": {
	           "type": "@client",
	           "message": "foobar"
	         }
	       }
	     }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			triggerID = obj.Path("$.data.id").String().NotEmpty().Raw()

			attrs := obj.Path("$.data.attributes").Object()
			attrs.HasValue("type", "@client")
			attrs.HasValue("worker", "client")
		})

		t.Run("LaunchAClientJob", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers/"+triggerID+"/launch").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			jobID = obj.Path("$.data.id").String().NotEmpty().Raw()

			obj.Path("$.data.type").IsEqual(consts.Jobs)
			attrs := obj.Path("$.data.attributes").Object()
			attrs.HasValue("worker", "client")
			attrs.HasValue("state", job.Running)
			attrs.Value("queued_at").String().AsDateTime(time.RFC3339)
			attrs.Value("started_at").String().AsDateTime(time.RFC3339)
		})

		t.Run("PatchAClientJob", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.PATCH("/jobs/"+jobID).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
	       "data": {
	         "attributes": {
	           "state": "errored",
	           "error": "LOGIN_FAILED"
	         }
	       }
	     }`)).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Path("$.data.type").IsEqual(consts.Jobs)
			attrs := obj.Path("$.data.attributes").Object()
			attrs.HasValue("worker", "client")
			attrs.HasValue("state", job.Errored)
			attrs.HasValue("error", "LOGIN_FAILED")
			attrs.Value("queued_at").String().AsDateTime(time.RFC3339)
			attrs.Value("started_at").String().AsDateTime(time.RFC3339)
			attrs.Value("finished_at").String().AsDateTime(time.RFC3339)
		})
	})

	t.Run("SendCampaignEmail", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		t.Run("WithoutPermissions", func(t *testing.T) {
			e.POST("/jobs/campaign-emails").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
			"arguments": {
			  "subject": "Some subject",
			  "parts": [
				{ "body": "Some content", "type": "text/plain" }
			  ]
			}
		  }
        }
      }`)).Expect().Status(403)

			emailerSvc.AssertNumberOfCalls(t, "SendCampaignEmail", 0)
		})

		t.Run("WithProperArguments", func(t *testing.T) {
			emailerSvc.
				On("SendCampaignEmail", testInstance, mock.Anything).
				Return(nil).
				Once()

			scope := strings.Join([]string{
				consts.Jobs + ":ALL:sendmail:worker",
			}, " ")
			token, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI", scope,
				"", time.Now())

			e.POST("/jobs/campaign-emails").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
			"arguments": {
			  "subject": "Some subject",
			  "parts": [
				{ "body": "Some content", "type": "text/plain" }
			  ]
			}
		  }
        }
      }`)).Expect().Status(204)

			emailerSvc.AssertCalled(t, "SendCampaignEmail", testInstance, &emailer.CampaignEmailCmd{
				Subject: "Some subject",
				Parts: []mail.Part{
					{Body: "Some content", Type: "text/plain"},
				},
			})
		})

		t.Run("WithMissingSubject", func(t *testing.T) {
			emailerSvc.
				On("SendCampaignEmail", testInstance, mock.Anything).
				Return(emailer.ErrMissingSubject).
				Once()

			scope := strings.Join([]string{
				consts.Jobs + ":ALL:sendmail:worker",
			}, " ")
			token, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI", scope,
				"", time.Now())

			e.POST("/jobs/campaign-emails").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
			"arguments": {
			  "parts": [
				{ "body": "Some content", "type": "text/plain" }
			  ]
			}
		  }
        }
      }`)).Expect().Status(400)

			emailerSvc.AssertCalled(t, "SendCampaignEmail", testInstance, &emailer.CampaignEmailCmd{
				Parts: []mail.Part{
					{Body: "Some content", Type: "text/plain"},
				},
			})
		})
	})
}
