// Command echo is a tiny upstream used to demo gatekeeper. It answers every
// request with a JSON description of what it received. Pass ?status=503 to
// force an error status, which is handy for watching circuit breakers trip.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

type echoResponse struct {
	Name       string              `json:"name"`
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Query      string              `json:"query,omitempty"`
	Headers    map[string][]string `json:"headers"`
	RemoteAddr string              `json:"remote_addr"`
	Time       time.Time           `json:"time"`
}

func main() {
	addr := flag.String("addr", envOr("ECHO_ADDR", ":8081"), "listen address")
	name := flag.String("name", envOr("ECHO_NAME", hostname()), "instance name reported in responses")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusOK
		if s := r.URL.Query().Get("status"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n >= 100 && n <= 599 {
				status = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", *name)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(echoResponse{
			Name:       *name,
			Method:     r.Method,
			Path:       r.URL.Path,
			Query:      r.URL.RawQuery,
			Headers:    r.Header,
			RemoteAddr: r.RemoteAddr,
			Time:       time.Now().UTC(),
		})
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("echo %q listening on %s", *name, *addr)
	log.Fatal(srv.ListenAndServe())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "echo"
	}
	return h
}
