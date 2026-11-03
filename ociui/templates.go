package ociui

import (
	"fmt"
	"text/template"

	"github.com/ohseeeye/oci"
)

var (
	headerTmpl *template.Template
	bodyTmpl   *template.Template
)

func init() {
	headerTmpl = template.Must(template.New("headerTemplate").Parse(headerTemplate))
	bodyTmpl = template.Must(template.New("bodyTemplate").Funcs(template.FuncMap{
		"repoURL": repoURL, "imageURL": imageURL, "referrersURL": referrersURL,
	}).Parse(bodyTemplate))
}

const (
	gcrane     = `<a class="mt" href="https://github.com/google/go-containerregistry/blob/main/cmd/gcrane/README.md">gcrane</a>`
	craneLink  = `<a class="mt" href="https://github.com/google/go-containerregistry/blob/main/cmd/crane/README.md">crane</a>`
	subLinkFmt = `<a class="mt" href="https://github.com/google/go-containerregistry/blob/main/cmd/crane/doc/crane_%s.md">%s</a>`
)

func crane(sub string) string {
	if sub == "" {
		return craneLink
	}

	subLink := fmt.Sprintf(subLinkFmt, sub, sub)
	return craneLink + " " + subLink
}

const (
	registryForms = `
<p>Browse repositories and artifacts in the configured registry.
References are repository-relative; do not include a registry host.</p>
<form action="/_open" method="GET" autocomplete="off" spellcheck="false">
<label>Image <input size="80" type="text" name="image" placeholder="team/widget:latest" required/></label>
<input type="submit" value="Explore image"/>
</form>
<form action="/_open" method="GET" autocomplete="off" spellcheck="false">
<label>Repository <input size="80" type="text" name="repo" placeholder="team/widget"/></label>
<input type="submit" value="List tags"/>
</form>
<p><a href="/">Repository catalog</a></p>
`

	headerTemplate = `
<html>
<head>
<title>{{.Title}}</title>
<link rel="icon" href="/favicon.svg">
<style>
.mt:hover {
	text-decoration: underline;
}

.mt {
	color: inherit;
	text-decoration: inherit;
}

.link {
	position: relative;
	bottom: .125em;
}

.crane {
	height: 1em;
	width: 1em;
}

.top {
	color: inherit;
	text-decoration: inherit;
}

:root {
  color-scheme: light dark;
}

body {
	font-family: monospace;
	width: fit-content;
	overflow-wrap: anywhere;
	padding: 12px;
}

pre {
	white-space: pre-wrap;
}

.indent {
	margin-left: 2em;
}

.noselect {
	user-select: none;
	-webkit-user-select: none;
	width: fit-content;
	overflow-wrap: none;
	padding-right: 1em;
	text-align: right;
	white-space: nowrap;
}

td {
	vertical-align: top;
}

// Adapted from https://medium.com/allenhwkim/how-to-build-tabs-only-with-css-844718d7de2f
input + label { display: inline-block } /* show labels in line */
input[type="radio"] { display: none; } /* hide radio buttons only */
input ~ .tab { display: none }          /* hide contents */

/* show contents only for selected tab */
#tab1:checked ~ .tab.content1,
#tab2:checked ~ .tab.content2 { display: block; }

input + label {             /* box with rounded corner */
	display: inline-block;
  border: 1px solid #999;
  padding: 4px 12px;
  border-radius: 4px 4px 0 0;
  position: relative;
  top: 1px;
  opacity: 50%;
}
input:checked + label {     /* white background for selected tab */
  opacity: 100%;
}
input ~ .tab {          /* grey line between tab and contents */
  border-top: 1px solid #999;
  padding-top: 0.5em;
}
</style>
</head>
`

	bodyTemplate = `
<body>
<div>
<h1><a class="top" href="/"><img class="crane" src="/favicon.svg"/> <span class="link">Registry Explorer</span></a></h1>
{{ if .Up }}
<h2><a class="mt" href="{{repoURL .Up.Parent}}">{{.Up.Parent}}</a>{{.Up.Separator}}{{if .ReferenceURL }}<a class="mt" href="{{.ReferenceURL}}">{{.Up.Child}}</a>{{else}}{{.Up.Child}}{{end}}{{ range .CosignTags }} (<a href="{{imageURL (printf "%s:%s" $.Repo .Tag)}}">{{.Short}}</a>){{end}}{{if .Referrers}} <a href="{{referrersURL $.Repo $.Descriptor.Digest.String}}">(referrers)</a>{{end}}</h2>
{{ else }}
	<h2>{{.Reference}}{{ range .CosignTags }} (<a href="{{imageURL (printf "%s:%s" $.Repo .Tag)}}">{{.Short}}</a>){{end}}{{if .Referrers}} <a href="{{referrersURL $.Repo $.Descriptor.Digest.String}}">(referrers)</a>{{end}}</h2>
{{ end }}
{{ if .Descriptor }}
<input type="radio" name="tabs" id="tab1" checked />
<label for="tab1">HTTP</label>
<input type="radio" name="tabs" id="tab2" />
<label for="tab2">OCI</label>
<div class="tab content1">
Content-Type: {{if .MediaTypeLink}}<a class="mt" href="{{.MediaTypeLink}}">{{.Descriptor.MediaType}}</a>{{else}}{{.Descriptor.MediaType}}{{end}}<br>
Docker-Content-Digest: <a class="mt" href="{{.DigestURL}}">{{.Descriptor.Digest}}</a><br>
<span{{if .HumanSize}} title="{{.HumanSize}}"{{end}}>Content-Length: {{if .SizeLink}}<a class="mt" href="{{.SizeLink}}">{{.Descriptor.Size}}</a>{{else}}{{.Descriptor.Size}}{{end}}</span><br>
{{if $.Subject}}OCI-Subject: <a class="mt" href="{{imageURL (printf "%s@%s" $.Repo .Subject)}}">{{.Subject}}</a><br>{{end}}
</div>
<div class="tab content2">
{<br>
&nbsp;&nbsp;"mediaType": "{{.Descriptor.MediaType}}",<br>
&nbsp;&nbsp;"digest": "{{.Descriptor.Digest}}",<br>
&nbsp;&nbsp;"size": {{.Descriptor.Size}}<br>
}<br>
</div>

{{end}}
</div>
{{ if .JQ }}
<h4><span style="padding:0;" class="noselect">$ </span>{{.JQ}}</h4>

{{ end }}
`

	footer = `
</body>
</html>
`
)

type RepoParent struct {
	Parent    string
	Child     string
	Separator string
}

type TitleData struct {
	Title string
}
type CosignTag struct {
	Tag   string
	Short string
}

type HeaderData struct {
	Repo          string
	CosignTags    []CosignTag
	JQ            string
	Reference     string
	Up            *RepoParent
	Descriptor    *oci.Descriptor
	ReferenceURL  string
	DigestURL     string
	MediaTypeLink string
	SizeLink      string
	HumanSize     string
	Referrers     bool
	Subject       string
}
