package main

import (
	"html/template"
	"net/http"
	"time"
)

// ui is the minimal web page: the last doctor report and plan, refreshed every minute.
var ui = template.Must(template.New("ui").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta http-equiv="refresh" content="60">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>arrglue</title>
<style>
 body{font:14px/1.4 system-ui,sans-serif;margin:0 auto;max-width:1100px;padding:16px;color:#222;background:#fff}
 @media(prefers-color-scheme:dark){body{color:#ddd;background:#111}}
 h1{font-size:20px;margin:0 0 4px} h2{font-size:15px;margin:24px 0 6px} .sub{opacity:.6;font-weight:normal}
 table{border-collapse:collapse;width:100%} td{padding:3px 8px 3px 0;vertical-align:top;font-family:ui-monospace,monospace;font-size:13px;white-space:pre-wrap}
 .OK,.noop,.create{color:#2a9d3a} .WARN,.skip,.update,.adopt{color:#c48a00} .ERROR{color:#d33}
 .lvl{width:1.5em;text-align:center;font-weight:bold} .muted{opacity:.6}
</style></head><body>
<h1>arrglue <span class="sub">{{.Self}}</span></h1>
<div class="muted">last cycle {{.At}} · {{.Errors}} error(s), {{.Warnings}} warning(s) · {{.Pending}} change(s) pending{{if .AutoApply}} (auto-apply on){{end}}
 · <a href="/v1/services">/v1/services</a> <a href="/v1/doctor">/v1/doctor</a> <a href="/v1/plan">/v1/plan</a></div>
{{if .Err}}<p class="ERROR">{{.Err}}</p>{{end}}
{{range .Sections}}{{if .Rows}}<h2>{{.Name}} <span class="sub">{{.Title}}</span></h2>
<table>{{range .Rows}}<tr><td class="lvl {{.Level}}">{{index $.Sym .Level}}</td>{{range .Cols}}<td>{{.}}</td>{{end}}</tr>{{end}}</table>{{end}}{{end}}
<h2>PLAN <span class="sub">what apply would change</span></h2>
<table>{{range .Changes}}<tr><td class="lvl {{.Op}}">{{index $.Sym .Op}}</td><td>{{.Link}}</td><td>{{.What}}</td><td class="muted">{{.Detail}}</td></tr>{{else}}<tr><td class="muted">nothing yet</td></tr>{{end}}</table>
</body></html>`))

var symbols = map[string]string{"OK": "✓", "WARN": "!", "ERROR": "✗", "create": "+", "update": "~", "adopt": "±", "skip": "-", "noop": "="}

func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type change struct{ Link, Op, What, Detail string }
	data := struct {
		Self, At, Err             string
		Errors, Warnings, Pending int
		AutoApply                 bool
		Sections                  []Section
		Changes                   []change
		Sym                       map[string]string
	}{Self: "not scanned yet", At: "never", AutoApply: s.autoApply, Sym: symbols}
	if !s.at.IsZero() {
		data.At = s.at.Format(time.RFC3339)
	}
	if s.err != nil {
		data.Err = s.err.Error()
	}
	if s.rep != nil {
		data.Self, data.Errors, data.Warnings, data.Sections = s.rep.Self, s.rep.Errors, s.rep.Warnings, s.rep.Sections
	}
	for _, c := range s.changes {
		if c.Apply != nil {
			data.Pending++
		}
		data.Changes = append(data.Changes, change{arrow(c.Link), c.Op, c.What, c.Detail})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	ui.Execute(w, data)
}
