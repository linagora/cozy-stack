package rag

import (
	"embed"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// The prompts of the chat router and of the actions are text templates, one
// file each in prompts/, so that their wording is read and edited as text.
//
//go:embed prompts/*.txt
var promptFiles embed.FS

var prompts = template.Must(template.New("").Funcs(template.FuncMap{
	"quote": strconv.Quote,
	"trim":  strings.TrimSpace,
}).ParseFS(promptFiles, "prompts/*.txt"))

// renderPrompt executes the prompt template of the given file name. The
// templates are parsed at startup and the data is fixed by the caller, so an
// error here is a programming error.
func renderPrompt(name string, data interface{}) string {
	var b strings.Builder
	if err := prompts.ExecuteTemplate(&b, name, data); err != nil {
		panic(err)
	}
	return b.String()
}

// promptDate is how the prompts tell the LLM the current date.
func promptDate(now time.Time) string {
	return now.Format("Monday, January 2, 2006")
}
