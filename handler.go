package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

type pushEnvelope struct {
	Message struct {
		MessageID   string            `json:"messageId"`
		PublishTime string            `json:"publishTime"`
		Data        string            `json:"data"`
		Attributes  map[string]string `json:"attributes"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

type Config struct {
	Project       string
	DefaultRegion string
}

type pushHandler struct {
	cfg    Config
	runner JobRunner
	log    *slog.Logger
}

func newPushHandler(cfg Config, runner JobRunner, log *slog.Logger) *pushHandler {
	return &pushHandler{cfg: cfg, runner: runner, log: log}
}

func (h *pushHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var env pushEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		h.log.Warn("malformed push envelope", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	logger := h.log.With(
		"message_id", env.Message.MessageID,
		"subscription", env.Subscription,
	)

	jobName := env.Message.Attributes["job_name"]
	if jobName == "" {
		logger.Warn("missing job_name attribute; dropping")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	logger = logger.With("job_name", jobName)

	region := env.Message.Attributes["job_region"]
	if region == "" {
		region = h.cfg.DefaultRegion
	}

	var args []string
	if raw, ok := env.Message.Attributes["job_args"]; ok {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			logger.Warn("malformed job_args attribute; dropping", "err", err, "raw", raw)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	overrides := JobOverrides{Env: buildEnv(env), Args: args}
	if err := h.runner.Run(r.Context(), h.cfg.Project, region, jobName, overrides); err != nil {
		var perm *PermanentError
		if errors.As(err, &perm) {
			logger.Error("permanent RunJob failure; dropping", "err", err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		logger.Error("transient RunJob failure; will retry", "err", err)
		http.Error(w, "run job failed", http.StatusServiceUnavailable)
		return
	}

	logger.Info("dispatched", "region", region, "args_overridden", args != nil)
	w.WriteHeader(http.StatusNoContent)
}

func buildEnv(env pushEnvelope) map[string]string {
	out := map[string]string{
		"PUBSUB_MESSAGE_ID":   env.Message.MessageID,
		"PUBSUB_PUBLISH_TIME": env.Message.PublishTime,
		"PUBSUB_DATA":         env.Message.Data,
	}
	for k, v := range env.Message.Attributes {
		out["PUBSUB_ATTR_"+strings.ToUpper(k)] = v
	}
	return out
}
