package apidocs

import (
	_ "embed"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

//go:embed index.html
var indexHTML []byte

//go:embed openapi.yaml
var openapiYAML []byte

func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /swagger/{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /swagger/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiYAML)
	})
	mux.HandleFunc("GET /swagger", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/", http.StatusMovedPermanently)
	})
	return mux
}

var (
	specPathRe   = regexp.MustCompile(`^  (/\S*):\s*$`)
	specMethodRe = regexp.MustCompile(`^    (get|post|put|delete|patch|head|options):\s*$`)
)

func SpecRoutes() []string {
	routes := make([]string, 0)
	current := ""
	inPaths := false
	for _, line := range strings.Split(string(openapiYAML), "\n") {
		if strings.HasPrefix(line, "paths:") {
			inPaths = true
			continue
		}
		if !inPaths {
			continue
		}
		if m := specPathRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if m := specMethodRe.FindStringSubmatch(line); m != nil && current != "" {
			routes = append(routes, strings.ToUpper(m[1])+" "+current)
		}
	}
	sort.Strings(routes)
	return routes
}
