package main

// The forge's HTTP server: health probe, git smart HTTP, REST v3,
// GraphQL v4 and MCP, behind the one bearer-token auth the suite
// speaks. main.go owns the lifecycle; this file is the whole surface.
//
// Requests are handled one at a time under a mutex: the store is a set
// of plain maps, and serial handling keeps every multi-step operation
// atomic exactly the way a single-threaded event loop would.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxBodyBytes = 128 * 1024 * 1024
const jsonContentType = "application/json; charset=utf-8"

var shopToken string

var requestCounter int64 = 0
var mutationGeneration int64 = 0

// requestMutex serializes request handling (see the package comment).
var requestMutex sync.Mutex

// nextRequestID mints forge-<counter base36>-<millis base36>; the
// counter starts at 1.
func nextRequestID() string {
	counter := atomic.AddInt64(&requestCounter, 1)
	millis := time.Now().UnixMilli()
	return "forge-" + strconv.FormatInt(counter, 36) + "-" + strconv.FormatInt(millis, 36)
}

func authorized(header string) bool {
	if shopToken == "" {
		// No token configured: any non-empty Bearer credential names the
		// viewer.
		return strings.HasPrefix(header, "Bearer ") && len(header) > len("Bearer ")
	}
	return header == "Bearer "+shopToken
}

// ---------------------------------------------------------------------------
// GET response cache
//
// Read traffic repeats itself: the same issue, repo and advertisement
// are fetched over and over between writes. A GET runs once; while the
// mutation generation stands still, later identical GETs replay the
// captured status, headers and body instead of re-running the handler.
// The request-id header is always minted fresh on a replay.

type capturedReply struct {
	generation int64
	status     int
	headers    map[string]string
	body       []byte
}

const getCacheLimit = 1024

var getResponseCache = map[string]*capturedReply{}

func storeCapturedReply(key string, generation int64, reply *capturedReply) {
	if len(getResponseCache) >= getCacheLimit {
		getResponseCache = map[string]*capturedReply{}
	}
	getResponseCache[key] = reply
}

// ---------------------------------------------------------------------------

// responder collects one reply inside dispatch; handleRequest writes it
// out (or replays a cached one) afterwards.
type responder struct {
	status  int
	headers map[string]string
	body    []byte
	sent    bool
}

func (rs *responder) send(status int, contentType string, body []byte) {
	if rs.sent {
		return
	}
	rs.sent = true
	rs.status = status
	rs.body = body
	if contentType != "" {
		rs.headers["Content-Type"] = contentType
	}
}

func (rs *responder) sendJSON(status int, value interface{}) {
	body, err := json.Marshal(value)
	if err != nil {
		body = []byte("null")
	}
	rs.send(status, jsonContentType, body)
}

// parseJSON decodes a request body the way JSON.parse would be used:
// any JSON value, with numbers as float64.
func parseJSON(raw []byte) (interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func marshalJSON(value interface{}) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func readRequestBody(r *http.Request) []byte {
	if r.Body == nil {
		return []byte{}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return []byte{}
	}
	return body
}

func writeReply(w http.ResponseWriter, requestID string, status int, headers map[string]string, body []byte) {
	header := w.Header()
	for key, value := range headers {
		header.Set(key, value)
	}
	header.Set("X-GitHub-Request-Id", requestID)
	if body != nil {
		header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	requestMutex.Lock()
	defer requestMutex.Unlock()
	defer func() {
		if recovered := recover(); recovered != nil {
			rs := &responder{headers: map[string]string{}}
			rs.sendJSON(http.StatusInternalServerError, map[string]interface{}{"message": "Internal Server Error"})
			writeReply(w, nextRequestID(), rs.status, rs.headers, rs.body)
		}
	}()

	requestID := nextRequestID()
	method := r.Method
	rawURL := r.URL.RequestURI()

	// GETs (except the health probe) go through the response cache;
	// unauthorized requests are never cached.
	if method == "GET" && !strings.HasPrefix(rawURL, "/healthz") && authorized(r.Header.Get("Authorization")) {
		key := r.Host + "\n" + rawURL
		generation := atomic.LoadInt64(&mutationGeneration)
		if hit, ok := getResponseCache[key]; ok && hit.generation == generation {
			writeReply(w, requestID, hit.status, hit.headers, hit.body)
			return
		}
		rs := dispatch(r)
		if (rs.status == 200 || rs.status == 404) && len(rs.body) <= 1024*1024 {
			storeCapturedReply(key, generation, &capturedReply{
				generation: generation, status: rs.status, headers: rs.headers, body: rs.body,
			})
		}
		writeReply(w, requestID, rs.status, rs.headers, rs.body)
		return
	}

	rs := dispatch(r)
	writeReply(w, requestID, rs.status, rs.headers, rs.body)
}

// dispatch routes one request and produces its reply.
func dispatch(r *http.Request) (rs *responder) {
	rs = &responder{status: 200, headers: map[string]string{}}
	defer func() {
		if recovered := recover(); recovered != nil {
			fresh := &responder{headers: map[string]string{}}
			fresh.sendJSON(http.StatusInternalServerError, map[string]interface{}{"message": "Internal Server Error"})
			*rs = *fresh
		}
	}()

	method := r.Method
	pathname := r.URL.Path

	// The env's boot check: no auth, always up.
	if method == "GET" && pathname == "/healthz" {
		rs.sendJSON(http.StatusOK, map[string]interface{}{"ok": true})
		return rs
	}

	needsBody := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
	var rawBody []byte
	if needsBody {
		rawBody = readRequestBody(r)
	}

	// Everything else speaks the one bearer token.
	if !authorized(r.Header.Get("Authorization")) {
		rs.sendJSON(http.StatusUnauthorized, map[string]interface{}{"message": "Bad credentials"})
		return rs
	}

	// GraphQL v4.
	if pathname == "/api/graphql" {
		if method != "POST" {
			rs.sendJSON(http.StatusNotFound, map[string]interface{}{"message": "Not Found"})
			return rs
		}
		parsed, err := parseJSON(rawBody)
		if err != nil {
			rs.sendJSON(http.StatusBadRequest, map[string]interface{}{"message": "Problems parsing JSON"})
			return rs
		}
		atomic.AddInt64(&mutationGeneration, 1)
		status, reply := handleGraphQL(parsed)
		rs.sendJSON(status, reply)
		return rs
	}

	// MCP.
	if pathname == "/mcp" {
		if method != "POST" {
			rs.sendJSON(http.StatusNotFound, map[string]interface{}{"message": "Not Found"})
			return rs
		}
		atomic.AddInt64(&mutationGeneration, 1)
		reply := handleMCP(rawBody)
		if !reply.hasBody {
			rs.status = reply.status
			rs.sent = true
			return rs
		}
		rs.sendJSON(reply.status, reply.body)
		return rs
	}

	// REST v3.
	if pathname == "/api/v3" || strings.HasPrefix(pathname, "/api/v3/") {
		tail := strings.TrimRight(pathname[len("/api/v3"):], "/")
		var segments []string
		if tail != "" {
			segments = strings.Split(strings.TrimPrefix(tail, "/"), "/")
		}
		var body map[string]interface{}
		bodyError := false
		if len(rawBody) > 0 {
			if parsed, err := parseJSON(rawBody); err != nil {
				bodyError = true
			} else if object, ok := parsed.(map[string]interface{}); ok {
				body = object
			} else {
				body = map[string]interface{}{}
			}
		}
		if method != "GET" {
			atomic.AddInt64(&mutationGeneration, 1)
		}
		request := &restRequest{
			Method: method, Segments: segments, Query: r.URL.Query(),
			Body: body, BodyError: bodyError, Host: r.Host,
		}
		reply := handleRest(request)
		for key, value := range reply.Headers {
			rs.headers[key] = value
		}
		if reply.Body == nil {
			rs.status = reply.Status
			rs.sent = true
			return rs
		}
		rs.sendJSON(reply.Status, reply.Body)
		return rs
	}

	// Git smart HTTP: /<owner>/<repo>.git/<endpoint>.
	if owner, repoName, endpoint, ok := parseGitPath(pathname); ok {
		repo := getRepo(owner, repoName)
		if repo == nil {
			rs.send(http.StatusNotFound, "text/plain; charset=utf-8", []byte("repository not found\n"))
			return rs
		}
		protocolV2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
		if endpoint == "info/refs" && method == "GET" {
			reply := handleInfoRefs(repo, r.URL.Query().Get("service"), protocolV2)
			rs.send(reply.status, reply.contentType, reply.body)
			return rs
		}
		if endpoint == "git-upload-pack" && method == "POST" {
			reply := handleUploadPack(repo, rawBody, protocolV2)
			rs.send(reply.status, reply.contentType, reply.body)
			return rs
		}
		if endpoint == "git-receive-pack" && method == "POST" {
			atomic.AddInt64(&mutationGeneration, 1)
			reply := handleReceivePack(repo, rawBody)
			rs.send(reply.status, reply.contentType, reply.body)
			return rs
		}
		rs.sendJSON(http.StatusNotFound, map[string]interface{}{"message": "Not Found"})
		return rs
	}

	rs.sendJSON(http.StatusNotFound, map[string]interface{}{"message": "Not Found"})
	return rs
}

// parseGitPath matches /<owner>/<repo>.git/<endpoint> where endpoint is
// one of the three smart-HTTP endpoints.
func parseGitPath(pathname string) (owner, repoName, endpoint string, ok bool) {
	trimmed := strings.TrimPrefix(pathname, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 {
		return "", "", "", false
	}
	endpoint = strings.Join(parts[2:], "/")
	if endpoint != "info/refs" && endpoint != "git-upload-pack" && endpoint != "git-receive-pack" {
		return "", "", "", false
	}
	if !strings.HasSuffix(parts[1], ".git") {
		return "", "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), endpoint, true
}
