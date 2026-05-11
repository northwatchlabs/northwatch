// Package ui exposes embedded HTML templates and static assets for the
// status page.
package ui

import (
	"embed"
	"html/template"
	"io/fs"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Templates returns the parsed template set. Render with
// `t.ExecuteTemplate(w, "layout", data)`.
func Templates() (*template.Template, error) {
	return template.ParseFS(templatesFS, "templates/*.html")
}

// StaticFS returns the embedded /static subtree rooted so it can be
// served at /static/* without the "static/" prefix.
func StaticFS() (fs.FS, error) {
	return fs.Sub(staticFS, "static")
}
