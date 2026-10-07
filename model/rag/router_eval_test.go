//go:build routereval

package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/stretchr/testify/require"
)

// TestRouterEval compares router configurations with a real openRAG: each
// case is routed by every configuration, several times, then the documents
// are checked like the stack does when an action is said not to need them.
//
//	ROUTER_EVAL_URL, ROUTER_EVAL_KEY: the openRAG server
//	ROUTER_EVAL_DOMAIN: the partition of the documents
//	ROUTER_EVAL_CASES: the JSON file of the cases
//	ROUTER_EVAL_CONFIGS: a JSON file of the configurations, a list of
//	  {"name", "mode", "override"}, override being the llm_override of
//	  openRAG (the JSON schema and tools modes with its default LLM)
//	ROUTER_EVAL_OUT: the JSON file of the results
//	ROUTER_EVAL_REPS: the number of runs of each case in each configuration (3)
//	ROUTER_EVAL_FILL: when set, the params of an action that does not need
//	  the documents are prepared like the stack does, from the tool call in
//	  the tools-params mode, else with a fill call
//
// A case may give its own actions, in place of testdata/chat_actions.json,
// and be direct, like a scribe: the action is then made without the
// documents. contains lists the texts its params must contain.
//
//	go test -tags routereval -run TestRouterEval -v -timeout 60m ./model/rag/
func TestRouterEval(t *testing.T) {
	config.UseTestFile(t)
	previous := config.GetConfig().RAGServers
	config.GetConfig().RAGServers = map[string]config.RAGServer{
		config.DefaultInstanceContext: {URL: os.Getenv("ROUTER_EVAL_URL"), APIKey: os.Getenv("ROUTER_EVAL_KEY")},
	}
	t.Cleanup(func() { config.GetConfig().RAGServers = previous })
	inst := &instance.Instance{Domain: os.Getenv("ROUTER_EVAL_DOMAIN")}
	actions := testActions(t)
	reps := 3
	if n, err := strconv.Atoi(os.Getenv("ROUTER_EVAL_REPS")); err == nil && n > 0 {
		reps = n
	}

	var cases []struct {
		// Messages alternate user and assistant, from the user
		Messages []string           `json:"messages"`
		Expected string             `json:"expected"`
		Docs     *bool              `json:"docs"`
		Actions  []ActionDefinition `json:"actions"`
		Direct   bool               `json:"direct"`
		Contains map[string]string  `json:"contains"`
	}
	raw, err := os.ReadFile(os.Getenv("ROUTER_EVAL_CASES"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &cases))
	fill := os.Getenv("ROUTER_EVAL_FILL") != ""

	configs := []struct {
		Name     string                 `json:"name"`
		Mode     string                 `json:"mode"`
		Override map[string]interface{} `json:"override,omitempty"`
	}{{Name: "schema", Mode: "schema"}, {Name: routerTools, Mode: routerTools}, {Name: routerToolsParams, Mode: routerToolsParams}}
	if path := os.Getenv("ROUTER_EVAL_CONFIGS"); path != "" {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &configs))
	}

	type run struct {
		Case    int     `json:"case"`
		Mode    string  `json:"mode"`
		Rep     int     `json:"rep"`
		Intent  string  `json:"intent"`
		Docs    bool    `json:"docs"`
		Checked bool    `json:"checked"`
		Error   string  `json:"error,omitempty"`
		Seconds float64 `json:"seconds"`
		// Confidence and Documents come from a JEV decision model
		Confidence float64 `json:"confidence,omitempty"`
		Documents  float64 `json:"documents,omitempty"`
		IntentOK   bool    `json:"intent_ok"`
		DocsOK     *bool   `json:"docs_ok,omitempty"`
		FinalOK    bool    `json:"final_ok"`
		// The params of an action that does not need the documents, from
		// the tool call or from a fill call, and their checks
		Params      map[string]interface{} `json:"params,omitempty"`
		ParamsFrom  string                 `json:"params_from,omitempty"`
		ParamsError string                 `json:"params_error,omitempty"`
		ParamsOK    *bool                  `json:"params_ok,omitempty"`
		FillSeconds float64                `json:"fill_seconds,omitempty"`
	}
	var runs []run
	relevant := map[string]bool{}
	var modes []string
	for _, c := range configs {
		modes = append(modes, c.Name)
	}
	for rep := 0; rep < reps; rep++ {
		for i, c := range cases {
			var messages []ragMessage
			for j, content := range c.Messages {
				role := UserRole
				if j%2 == 1 {
					role = AssistantRole
				}
				messages = append(messages, ragMessage{Role: role, Content: content})
			}
			last := c.Messages[len(c.Messages)-1]
			caseActions := actions
			if len(c.Actions) > 0 {
				caseActions = c.Actions
			}
			// The configurations run in turn, from another one on each case
			for k := range configs {
				cfg := configs[(k+rep+i)%len(configs)]
				started := time.Now()
				d, err := route(context.Background(), inst, cfg.Mode, messages, caseActions, cfg.Override)
				r := run{Case: i, Mode: cfg.Name, Rep: rep, Intent: d.Intent, Docs: d.NeedsDocuments, Seconds: time.Since(started).Seconds(),
					Confidence: d.confidence, Documents: d.documents}
				if err != nil {
					r.Error = err.Error()
				}
				if c.Direct {
					d.NeedsDocuments = false
				}
				final := d.NeedsDocuments
				if d.action() != "" && !d.NeedsDocuments && !c.Direct {
					found, ok := relevant[last]
					if !ok {
						found, err = hasRelevantDocuments(context.Background(), inst, last, "")
						found = found || err != nil
						relevant[last] = found
					}
					r.Checked = found
					final = found
				}
				r.IntentOK = d.Intent == c.Expected
				if c.Docs != nil && c.Expected != searchIntent {
					ok := r.IntentOK && d.NeedsDocuments == *c.Docs
					r.DocsOK = &ok
				}
				r.FinalOK = r.IntentOK && (c.Docs == nil || c.Expected == searchIntent || final == *c.Docs)
				if def := actionFor(caseActions, d.action()); fill && r.IntentOK && def != nil && def.Parameters != nil && !final {
					r.ParamsFrom = "fill"
					if d.params != nil {
						r.ParamsFrom = "call"
						if _, err := checkParams(def, d.params, userText(messages)); err != nil {
							r.ParamsFrom = "fill after the call"
						}
					}
					started := time.Now()
					d.NeedsDocuments = false
					a, err := prepareAction(context.Background(), inst, TestingLogger(), def, d, messages, cfg.Override, time.Now().UTC())
					r.FillSeconds = time.Since(started).Seconds()
					ok := err == nil
					if err != nil {
						r.ParamsError = err.Error()
					} else {
						r.Params = a.Params
						for name, text := range c.Contains {
							value := fmt.Sprint(a.Params[name])
							ok = ok && strings.Contains(strings.ToLower(value), strings.ToLower(text))
						}
					}
					r.ParamsOK = &ok
				}
				runs = append(runs, r)
				fmt.Printf("rep %d case %2d %-12s %-16s docs=%-5t checked=%-5t %.2fs %s conf=%.2f pdocs=%.2f %s\n", rep, i, cfg.Name, r.Intent, r.Docs, r.Checked, r.Seconds, mark(r.FinalOK), r.Confidence, r.Documents, r.Error)
				if r.ParamsOK != nil {
					params, _ := json.Marshal(r.Params)
					fmt.Printf("    params from %s in %.2fs %s %s%s\n", r.ParamsFrom, r.FillSeconds, mark(*r.ParamsOK), params, r.ParamsError)
				}
			}
		}
	}

	fmt.Printf("\n%d cases, %d runs per mode and case\n", len(cases), reps)
	for _, mode := range modes {
		var intentOK, docsOK, docsTotal, finalOK, errs, total int
		var seconds []float64
		for _, r := range runs {
			if r.Mode != mode {
				continue
			}
			total++
			if r.IntentOK {
				intentOK++
			}
			if r.DocsOK != nil {
				docsTotal++
				if *r.DocsOK {
					docsOK++
				}
			}
			if r.FinalOK {
				finalOK++
			}
			if r.Error != "" {
				errs++
			}
			seconds = append(seconds, r.Seconds)
		}
		sort.Float64s(seconds)
		fmt.Printf("%-12s intent %d/%d, needs_documents %d/%d, after the check %d/%d, errors %d, latency median %.2fs p90 %.2fs max %.2fs\n",
			mode, intentOK, total, docsOK, docsTotal, finalOK, total, errs,
			seconds[len(seconds)/2], seconds[len(seconds)*9/10], seconds[len(seconds)-1])
		var prepared, paramsOK, fromCall int
		var ready []float64
		for _, r := range runs {
			if r.Mode != mode || r.ParamsOK == nil {
				continue
			}
			prepared++
			if *r.ParamsOK {
				paramsOK++
			}
			if r.ParamsFrom == "call" {
				fromCall++
			}
			ready = append(ready, r.Seconds+r.FillSeconds)
		}
		if prepared > 0 {
			sort.Float64s(ready)
			fmt.Printf("%-12s params %d/%d ok, %d from the tool call, action ready median %.2fs p90 %.2fs max %.2fs\n",
				"", paramsOK, prepared, fromCall, ready[len(ready)/2], ready[len(ready)*9/10], ready[len(ready)-1])
		}
	}
	fmt.Println("\nCases where a mode is not always right:")
	for i, c := range cases {
		line := ""
		for _, mode := range modes {
			outcomes := ""
			wrong := false
			for _, r := range runs {
				if r.Case == i && r.Mode == mode {
					outcomes += fmt.Sprintf(" %s(%t)%s", r.Intent, r.Docs, mark(r.FinalOK))
					wrong = wrong || !r.FinalOK
				}
			}
			if wrong {
				line += fmt.Sprintf("\n    %-12s%s", mode, outcomes)
			}
		}
		if line != "" {
			fmt.Printf("  %2d %q expected %s%s\n", i, c.Messages[len(c.Messages)-1], c.Expected, line)
		}
	}
	if out := os.Getenv("ROUTER_EVAL_OUT"); out != "" {
		raw, err := json.MarshalIndent(runs, "", " ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(out, raw, 0o644))
	}
}

func mark(ok bool) string {
	if ok {
		return "ok"
	}
	return "KO"
}
