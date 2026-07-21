package handlers

import (
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/evgenza/otus-app/internal/version"
)

//go:embed status.html
var statusHTML string

var (
	statusTmpl = template.Must(template.New("status").Parse(statusHTML))
	startedAt  = time.Now()
)

type statusData struct {
	Version      string
	BuildDate    string
	Uptime       string
	Now          string
	DBOK         bool
	DBError      string
	Total        int
	BadChecksums int
	TLSEnabled   bool
	AuthEnabled  bool
}

func (a *API) statusPage(w http.ResponseWriter, r *http.Request) {
	data := statusData{
		Version:     version.Version,
		BuildDate:   version.Date,
		Uptime:      time.Since(startedAt).Round(time.Second).String(),
		Now:         time.Now().Format("02.01.2006 15:04:05"),
		TLSEnabled:  os.Getenv("TLS_CERT_FILE") != "",
		AuthEnabled: a.authEnabled,
	}

	msgs, err := a.store.List(r.Context())
	if err != nil {
		data.DBError = err.Error()
	} else {
		data.DBOK = true
		data.Total = len(msgs)
		for _, m := range msgs {
			if !m.ChecksumOK {
				data.BadChecksums++
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusTmpl.Execute(w, data); err != nil {
		slog.ErrorContext(r.Context(), "не удалось отрисовать статусную страницу", "err", err)
	}
}
