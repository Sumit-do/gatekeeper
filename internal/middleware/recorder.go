package middleware

import "net/http"

// Recorder wraps an http.ResponseWriter and records the status code and the
// number of body bytes written. It forwards Flush and exposes Unwrap so that
// http.ResponseController keeps working through it.
type Recorder struct {
	http.ResponseWriter
	status  int
	bytes   int64
	written bool
}

// NewRecorder wraps w. Until WriteHeader is called Status reports 200.
func NewRecorder(w http.ResponseWriter) *Recorder {
	return &Recorder{ResponseWriter: w, status: http.StatusOK}
}

// WriteHeader records code and forwards it once; later calls are ignored.
func (r *Recorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.written = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Write forwards the body, implicitly sending a 200 header if needed.
func (r *Recorder) Write(p []byte) (int, error) {
	if !r.written {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing.
func (r *Recorder) Flush() {
	if !r.written {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the wrapped writer for http.ResponseController.
func (r *Recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Status returns the recorded status code.
func (r *Recorder) Status() int { return r.status }

// Bytes returns the number of body bytes written so far.
func (r *Recorder) Bytes() int64 { return r.bytes }

// Written reports whether the header has been sent.
func (r *Recorder) Written() bool { return r.written }
