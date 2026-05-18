package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls []runCall
	err   error
}

type runCall struct {
	project, region, jobName string
	overrides                JobOverrides
}

func (f *fakeRunner) Run(_ context.Context, project, region, jobName string, o JobOverrides) error {
	f.calls = append(f.calls, runCall{project, region, jobName, o})
	return f.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func buildBody(t *testing.T, attrs map[string]string, data string) string {
	t.Helper()
	env := map[string]any{
		"subscription": "projects/p/subscriptions/s",
		"message": map[string]any{
			"messageId":   "msg-1",
			"publishTime": "2026-05-19T03:00:00Z",
			"data":        data,
			"attributes":  attrs,
		},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPushHandler(t *testing.T) {
	cfg := Config{Project: "myproj", DefaultRegion: "us-central1"}

	tests := []struct {
		name           string
		body           string
		runnerErr      error
		method         string
		wantStatus     int
		wantCalled     bool
		wantJobName    string
		wantRegion     string
		wantArgs       []string
		wantEnvIncludes map[string]string
	}{
		{
			name:        "happy path with env only",
			body:        buildBody(t, map[string]string{"job_name": "daily-report", "tenant": "acme"}, "aGVsbG8="),
			wantStatus:  http.StatusNoContent,
			wantCalled:  true,
			wantJobName: "daily-report",
			wantRegion:  "us-central1",
			wantEnvIncludes: map[string]string{
				"PUBSUB_MESSAGE_ID":   "msg-1",
				"PUBSUB_DATA":         "aGVsbG8=",
				"PUBSUB_ATTR_JOB_NAME": "daily-report",
				"PUBSUB_ATTR_TENANT":  "acme",
			},
		},
		{
			name:        "region override from attribute",
			body:        buildBody(t, map[string]string{"job_name": "j", "job_region": "asia-east1"}, ""),
			wantStatus:  http.StatusNoContent,
			wantCalled:  true,
			wantJobName: "j",
			wantRegion:  "asia-east1",
		},
		{
			name:        "args override from job_args",
			body:        buildBody(t, map[string]string{"job_name": "j", "job_args": `["--date=2026-05-19","--tenant=acme"]`}, ""),
			wantStatus:  http.StatusNoContent,
			wantCalled:  true,
			wantJobName: "j",
			wantRegion:  "us-central1",
			wantArgs:    []string{"--date=2026-05-19", "--tenant=acme"},
		},
		{
			name:       "malformed json -> 400, runner not called",
			body:       "{not json",
			wantStatus: http.StatusBadRequest,
			wantCalled: false,
		},
		{
			name:       "missing job_name -> 204 drop",
			body:       buildBody(t, map[string]string{"other": "x"}, ""),
			wantStatus: http.StatusNoContent,
			wantCalled: false,
		},
		{
			name:       "malformed job_args -> 204 drop",
			body:       buildBody(t, map[string]string{"job_name": "j", "job_args": "not-json"}, ""),
			wantStatus: http.StatusNoContent,
			wantCalled: false,
		},
		{
			name:       "permanent runner error -> 204",
			body:       buildBody(t, map[string]string{"job_name": "j"}, ""),
			runnerErr:  &PermanentError{Err: errors.New("not found")},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "transient runner error -> 503",
			body:       buildBody(t, map[string]string{"job_name": "j"}, ""),
			runnerErr:  errors.New("network"),
			wantStatus: http.StatusServiceUnavailable,
			wantCalled: true,
		},
		{
			name:       "GET -> 405",
			method:     http.MethodGet,
			body:       "",
			wantStatus: http.StatusMethodNotAllowed,
			wantCalled: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{err: tc.runnerErr}
			h := newPushHandler(cfg, runner, discardLogger())

			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			req := httptest.NewRequest(method, "/", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCalled && len(runner.calls) != 1 {
				t.Fatalf("expected 1 RunJob call, got %d", len(runner.calls))
			}
			if !tc.wantCalled && len(runner.calls) != 0 {
				t.Fatalf("expected no RunJob calls, got %d", len(runner.calls))
			}
			if !tc.wantCalled {
				return
			}
			c := runner.calls[0]
			if c.project != "myproj" {
				t.Errorf("project: got %q want myproj", c.project)
			}
			if tc.wantJobName != "" && c.jobName != tc.wantJobName {
				t.Errorf("jobName: got %q want %q", c.jobName, tc.wantJobName)
			}
			if tc.wantRegion != "" && c.region != tc.wantRegion {
				t.Errorf("region: got %q want %q", c.region, tc.wantRegion)
			}
			if tc.wantArgs != nil && !reflect.DeepEqual(c.overrides.Args, tc.wantArgs) {
				t.Errorf("args: got %v want %v", c.overrides.Args, tc.wantArgs)
			}
			if tc.wantArgs == nil && c.overrides.Args != nil {
				t.Errorf("args: got %v want nil", c.overrides.Args)
			}
			for k, v := range tc.wantEnvIncludes {
				if c.overrides.Env[k] != v {
					t.Errorf("env[%s]: got %q want %q", k, c.overrides.Env[k], v)
				}
			}
		})
	}
}

func TestBuildEnvUppercasesAttributes(t *testing.T) {
	env := pushEnvelope{}
	env.Message.MessageID = "id"
	env.Message.PublishTime = "t"
	env.Message.Data = "d"
	env.Message.Attributes = map[string]string{"job_name": "n", "Tenant-Id": "x"}

	got := buildEnv(env)
	want := map[string]string{
		"PUBSUB_MESSAGE_ID":     "id",
		"PUBSUB_PUBLISH_TIME":   "t",
		"PUBSUB_DATA":           "d",
		"PUBSUB_ATTR_JOB_NAME":  "n",
		"PUBSUB_ATTR_TENANT-ID": "x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildEnv mismatch\n got: %v\nwant: %v", got, want)
	}
}
