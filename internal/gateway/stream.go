package gateway

import (
	"io"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/statistics"
)

type StreamResult struct {
	Complete        bool
	BytesWritten    int64
	Error           error
	DownstreamError bool
	TTFB            time.Duration
	Usage           *statistics.Usage
	UsageExpected   bool
	// Stream reports whether the response was a server-sent event stream, so
	// the access log can tell a streaming call from a buffered one.
	Stream bool
}

// stallTimeout bounds how long a body read may sit without producing data.
// The gateway client only sets ResponseHeaderTimeout, so an upstream that
// sends headers and then stalls would otherwise pin the connection, the
// handler goroutine and the metrics concurrency slot forever.
const stallTimeout = 5 * time.Minute

// stallTimeoutReader closes the underlying body when no data has been read
// for the configured interval. Closing from another goroutine unblocks a
// pending Read, so copyStream terminates instead of hanging.
type stallTimeoutReader struct {
	r     io.ReadCloser
	delay time.Duration
	timer *time.Timer
	mu    sync.Mutex
	done  bool
}

func newStallTimeoutReader(r io.ReadCloser, delay time.Duration) *stallTimeoutReader {
	s := &stallTimeoutReader{r: r, delay: delay}
	s.timer = time.AfterFunc(delay, s.timeout)
	return s
}

func (s *stallTimeoutReader) timeout() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	_ = s.r.Close()
}

func (s *stallTimeoutReader) Read(p []byte) (int, error) {
	s.mu.Lock()
	if !s.done {
		if !s.timer.Stop() {
			// The timer already fired (or is about to): the timeout
			// goroutine is closing the body, so this Read will fail
			// shortly. Reset would schedule a second callback; do not.
			s.mu.Unlock()
			return s.r.Read(p)
		}
		s.timer.Reset(s.delay)
	}
	s.mu.Unlock()
	return s.r.Read(p)
}

func (s *stallTimeoutReader) Close() error {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	s.timer.Stop()
	return s.r.Close()
}

func copyResponse(w http.ResponseWriter, resp *http.Response, model catalog.Model, started time.Time) StreamResult {
	body := newStallTimeoutReader(resp.Body, stallTimeout)
	defer body.Close()
	for key, values := range resp.Header {
		if isHopByHop(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Free-Router-Provider", model.Provider)
	w.Header().Set("X-Free-Router-Model", model.UpstreamID)
	w.WriteHeader(resp.StatusCode)
	contentType := resp.Header.Get("Content-Type")
	eventStream := strings.Contains(contentType, "text/event-stream")
	result := copyBody(w, body, started, eventStream)
	result.UsageExpected = responseMayReportUsage(contentType)
	result.Stream = eventStream
	return result
}

func copyBody(w http.ResponseWriter, body io.Reader, started time.Time, flush bool) StreamResult {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	var totalWritten int64
	var ttfb time.Duration
	collector := &usageCollector{}
	headerTTFB := time.Since(started)
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			collector.Write(buffer[:read])
			if ttfb == 0 {
				ttfb = time.Since(started)
			}
			n, writeErr := w.Write(buffer[:read])
			totalWritten += int64(n)
			if writeErr != nil {
				return StreamResult{
					Complete:        false,
					BytesWritten:    totalWritten,
					Error:           writeErr,
					DownstreamError: true,
					TTFB:            ttfb,
					Usage:           collector.Usage(flush),
				}
			}
			if flush && canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if ttfb == 0 {
				ttfb = headerTTFB
			}
			return StreamResult{
				Complete:     err == io.EOF,
				BytesWritten: totalWritten,
				Error:        err,
				TTFB:         ttfb,
				Usage:        collector.Usage(flush),
			}
		}
	}
}

func isHopByHop(header string) bool {
	switch textproto.CanonicalMIMEHeaderKey(header) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}
