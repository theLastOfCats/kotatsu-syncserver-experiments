package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const (
	goldenDir   = "fixtures/golden"
	observedDir = "fixtures/observed"
)

// Used by stories that must send invalid JSON rather than a JSON string.
type rawJSON string

var storyName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
var mariaDBConnectionID = regexp.MustCompile(`\(conn=[0-9]+\)`)

type snapshotStep struct {
	Name     string `json:"name"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Status   int    `json:"status"`
	BodyKind string `json:"body_kind"`
	Body     any    `json:"body"`
}

type snapshot struct {
	Story string         `json:"story"`
	Steps []snapshotStep `json:"steps"`
}

type run struct {
	baseURL     string
	mailpitURL  string
	referenceDB string
	client      *http.Client
	stories     []snapshot
	current     int
	stepName    string
	secrets     []string
}

type failure struct{ message string }

func (r *run) require(ok bool, format string, args ...any) {
	if ok {
		return
	}
	context := "harness"
	if r.current >= 0 {
		context = "story " + r.stories[r.current].Story
		if r.stepName != "" {
			context += ", step " + r.stepName
		}
	}
	message := context + ": " + fmt.Sprintf(format, args...)
	for _, secret := range r.secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	panic(failure{message})
}

func (r *run) story(name string) {
	r.stepName = ""
	r.require(storyName.MatchString(name), "invalid story name")
	for _, existing := range r.stories {
		r.require(existing.Story != name, "duplicate story name %q", name)
	}
	r.stories = append(r.stories, snapshot{Story: name, Steps: []snapshotStep{}})
	r.current = len(r.stories) - 1
}

// rememberSecrets keeps diagnostics from exposing credentials even when an assertion
// accidentally includes one. Request bodies are never written to snapshots.
func (r *run) rememberSecrets(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") {
				if s, ok := item.(string); ok && s != "" {
					r.secrets = append(r.secrets, s)
				}
			}
			r.rememberSecrets(item)
		}
	case []any:
		for _, item := range v {
			r.rememberSecrets(item)
		}
	}
}

func (r *run) step(name, method, path, token string, body any, status int) any {
	r.require(r.current >= 0, "step called before story")
	r.stepName = name
	r.require(name != "", "step name must not be empty")
	r.require(strings.HasPrefix(path, "/"), "request path must start with /")
	if token != "" {
		r.secrets = append(r.secrets, token)
	}
	var requestBody io.Reader
	switch value := body.(type) {
	case nil:
	case rawJSON:
		requestBody = strings.NewReader(string(value))
	default:
		encoded, err := json.Marshal(value)
		r.require(err == nil, "cannot encode request body")
		var decoded any
		if json.Unmarshal(encoded, &decoded) == nil {
			r.rememberSecrets(decoded)
		}
		requestBody = bytes.NewReader(encoded)
	}
	// Keep URLs in snapshots free of credentials; stories supply tokens in headers or JSON.
	req, err := http.NewRequest(method, r.baseURL+path, requestBody)
	r.require(err == nil, "cannot construct HTTP request")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	r.require(err == nil, "HTTP request failed")
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	r.require(err == nil, "cannot read HTTP response")
	r.require(resp.StatusCode == status, "expected HTTP status %d, got %d", status, resp.StatusCode)
	r.require(status != http.StatusNoContent || len(data) == 0, "HTTP 204 response has a body")

	kind := "empty"
	var raw any
	if len(data) != 0 {
		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
			kind = "json"
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			err = decoder.Decode(&raw)
			r.require(err == nil, "invalid JSON response")
			var extra any
			r.require(decoder.Decode(&extra) == io.EOF, "JSON response contains trailing data")
		} else {
			kind = "text"
			raw = string(data)
		}
	}

	requestPath := "/" + strings.TrimLeft(strings.SplitN(path, "?", 2)[0], "/")
	// Copy the top-level object before normalization: stories inspect the raw response.
	observed := raw
	if r.referenceDB != "" && kind == "text" && status == http.StatusInternalServerError {
		// A connection number identifies pool scheduling, not application behavior.
		observed = mariaDBConnectionID.ReplaceAllString(raw.(string), "(conn=[ID])")
	}
	if object, ok := raw.(map[string]any); ok {
		copyObject := make(map[string]any, len(object))
		for key, value := range object {
			copyObject[key] = value
		}
		observed = copyObject
		if requestPath == "/auth" {
			if value, exists := object["token"]; exists {
				secret, ok := value.(string)
				r.require(ok && secret != "", "auth response token must be a nonempty string")
				r.secrets = append(r.secrets, secret)
				copyObject["token"] = "[REDACTED]"
			} else {
				r.require(status < 200 || status >= 300, "successful auth response is missing its token")
			}
		}
		if requestPath == "/resource/favourites" || requestPath == "/resource/history" {
			if number, ok := object["timestamp"].(json.Number); ok {
				if n, err := number.Int64(); err == nil && n > 0 {
					copyObject["timestamp"] = "[TIMESTAMP]"
				}
			}
		}
	} else if requestPath == "/auth" {
		r.require(status < 200 || status >= 300, "successful auth response must be an object with a token")
	}
	r.stories[r.current].Steps = append(r.stories[r.current].Steps, snapshotStep{
		Name: name, Method: method, Path: path, Status: resp.StatusCode, BodyKind: kind, Body: observed,
	})
	return raw
}

func parseSnapshot(data []byte) (snapshot, error) {
	var result snapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return result, errors.New("trailing data")
	}
	return result, nil
}

func verify(dir string, stories []snapshot) error {
	for _, actual := range stories {
		data, err := os.ReadFile(filepath.Join(dir, actual.Story+".json"))
		if err != nil {
			return fmt.Errorf("story %s: cannot read golden fixture", actual.Story)
		}
		expected, err := parseSnapshot(data)
		if err != nil {
			return fmt.Errorf("story %s: invalid golden fixture", actual.Story)
		}
		if expected.Story != actual.Story {
			return fmt.Errorf("story %s: fixture story name differs", actual.Story)
		}
		for i, step := range actual.Steps {
			if i >= len(expected.Steps) {
				return fmt.Errorf("story %s, step %d (%s): missing from fixture", actual.Story, i+1, step.Name)
			}
			want := expected.Steps[i]
			if want.Name != step.Name || want.Method != step.Method || want.Path != step.Path {
				return fmt.Errorf("story %s, step %d (%s): step identity differs", actual.Story, i+1, step.Name)
			}
			if want.Status != step.Status {
				return fmt.Errorf("story %s, step %d (%s): status differs (fixture %d, actual %d)", actual.Story, i+1, step.Name, want.Status, step.Status)
			}
			if want.BodyKind != step.BodyKind {
				return fmt.Errorf("story %s, step %d (%s): body kind differs", actual.Story, i+1, step.Name)
			}
			if !reflect.DeepEqual(want.Body, step.Body) {
				return fmt.Errorf("story %s, step %d (%s): response body differs", actual.Story, i+1, step.Name)
			}
		}
		if len(expected.Steps) != len(actual.Steps) {
			return fmt.Errorf("story %s: fixture has an extra step at position %d", actual.Story, len(actual.Steps)+1)
		}
	}
	return nil
}

func record(dir string, stories []snapshot) error {
	// Encode everything before touching the fixture directory.
	encoded := make([][]byte, len(stories))
	for i, story := range stories {
		data, err := json.MarshalIndent(story, "", "  ")
		if err != nil {
			return fmt.Errorf("story %s: cannot encode fixture", story.Story)
		}
		encoded[i] = append(data, '\n')
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return errors.New("cannot create fixture directory")
	}
	for i, story := range stories {
		file, err := os.CreateTemp(dir, ".record-*")
		if err != nil {
			return fmt.Errorf("story %s: cannot create fixture", story.Story)
		}
		name := file.Name()
		_, writeErr := file.Write(encoded[i])
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			os.Remove(name)
			return fmt.Errorf("story %s: cannot write fixture", story.Story)
		}
		if err := os.Chmod(name, 0644); err != nil {
			os.Remove(name)
			return fmt.Errorf("story %s: cannot set fixture permissions", story.Story)
		}
		if err := os.Rename(name, filepath.Join(dir, story.Story+".json")); err != nil {
			os.Remove(name)
			return fmt.Errorf("story %s: cannot replace fixture", story.Story)
		}
	}
	return nil
}

const usage = "usage: go run . verify|record --base-url URL [--suite sync|recovery|observations|history-policy|favourites-policy] [--mailpit-url URL] [--reference-db CONTAINER]"

func isOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") &&
		parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
		(parsed.Path == "" || parsed.Path == "/") && !parsed.ForceQuery
}

func execute(args []string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if f, ok := recovered.(failure); ok {
				err = errors.New(f.message)
			} else {
				panic(recovered)
			}
		}
	}()
	if len(args) == 0 || (args[0] != "record" && args[0] != "verify") {
		return errors.New(usage)
	}
	mode := args[0]
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	baseURL := flags.String("base-url", "", "backend base URL")
	suite := flags.String("suite", "sync", "scenario suite")
	mailpit := flags.String("mailpit-url", "", "disposable Mailpit API origin")
	referenceDB := flags.String("reference-db", "", "labelled disposable MariaDB fixture for observations")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return errors.New(usage)
	}
	if !isOrigin(*baseURL) {
		return errors.New("base URL must be an http(s) origin without credentials, path, query, or fragment")
	}
	if *suite != "sync" && *suite != "recovery" && *suite != "observations" && *suite != "history-policy" && *suite != "favourites-policy" {
		return fmt.Errorf("unknown suite %q", *suite)
	}
	if (*suite == "history-policy" || *suite == "favourites-policy") && mode != "verify" {
		return fmt.Errorf("%s is an assertion-only acceptance suite; record is not supported", *suite)
	}
	if (*suite == "recovery" || *suite == "observations") && !isOrigin(*mailpit) {
		return errors.New("recovery and observations suites require a disposable --mailpit-url origin")
	}
	if (*referenceDB != "") != (*suite == "observations") {
		return errors.New("--reference-db is required only for the observations suite")
	}
	r := &run{
		baseURL:     strings.TrimSuffix(*baseURL, "/"),
		mailpitURL:  strings.TrimSuffix(*mailpit, "/"),
		referenceDB: *referenceDB,
		current:     -1,
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	dir := goldenDir
	switch *suite {
	case "sync":
		scenarios(r)
		additionalSync(r)
	case "recovery":
		recoveryScenarios(r)
	case "history-policy":
		historyPolicy(r)
	case "favourites-policy":
		favouritesPolicy(r)
	case "observations":
		dir = observedDir
		syncObservations(r)
		recoveryObservations(r)
	}
	r.require(len(r.stories) > 0, "no stories were run")
	// Normative assertions are independent of reference-server snapshots.
	if *suite != "history-policy" && *suite != "favourites-policy" {
		if mode == "record" {
			err = record(dir, r.stories)
		} else {
			err = verify(dir, r.stories)
		}
	}
	if err != nil {
		return err
	}
	steps := 0
	for _, story := range r.stories {
		steps += len(story.Steps)
	}
	fmt.Printf("%s %s: %d scenarios, %d HTTP steps\n", mode, *suite, len(r.stories), steps)
	return nil
}

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
