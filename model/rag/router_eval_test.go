//go:build routereval

package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
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
		Messages []string `json:"messages"`
		Expected string   `json:"expected"`
		Docs     *bool    `json:"docs"`
	}
	raw, err := os.ReadFile(os.Getenv("ROUTER_EVAL_CASES"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &cases))

	configs := []struct {
		Name     string                 `json:"name"`
		Mode     string                 `json:"mode"`
		Override map[string]interface{} `json:"override,omitempty"`
	}{{Name: "schema", Mode: "schema"}, {Name: routerTools, Mode: routerTools}}
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
			// The configurations run in turn, from another one on each case
			for k := range configs {
				cfg := configs[(k+rep+i)%len(configs)]
				started := time.Now()
				d, err := route(context.Background(), inst, cfg.Mode, messages, actions, cfg.Override)
				r := run{Case: i, Mode: cfg.Name, Rep: rep, Intent: d.Intent, Docs: d.NeedsDocuments, Seconds: time.Since(started).Seconds(),
					Confidence: d.confidence, Documents: d.documents}
				if err != nil {
					r.Error = err.Error()
				}
				final := d.NeedsDocuments
				if d.action() != "" && !d.NeedsDocuments {
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
				runs = append(runs, r)
				fmt.Printf("rep %d case %2d %-8s %-16s docs=%-5t checked=%-5t %.2fs %s conf=%.2f pdocs=%.2f %s\n", rep, i, cfg.Name, r.Intent, r.Docs, r.Checked, r.Seconds, mark(r.FinalOK), r.Confidence, r.Documents, r.Error)
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
		fmt.Printf("%-8s intent %d/%d, needs_documents %d/%d, after the check %d/%d, errors %d, latency median %.2fs p90 %.2fs max %.2fs\n",
			mode, intentOK, total, docsOK, docsTotal, finalOK, total, errs,
			seconds[len(seconds)/2], seconds[len(seconds)*9/10], seconds[len(seconds)-1])
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
				line += fmt.Sprintf("\n    %-8s%s", mode, outcomes)
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
