package server

import (
	"log/slog"
	"sort"
	"strings"
)

// Audit writes one structured audit record: event=audit, actor, method,
// params and outcome. Every mutating call goes through it; the daemon
// injects the same JSON logger (with the optional file handler) it already
// uses, so audit lines land in journald and pfkeeper.log.
func Audit(logger *slog.Logger, actorCN, method string, params map[string]string, outcome string) {
	if logger == nil {
		return
	}
	attrs := make([]any, 0, 8)
	attrs = append(attrs, "event", "audit", "actor", actorCN, "method", method)
	if len(params) > 0 {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic audit lines (greppable, testable)
		pairs := make([]string, 0, len(params))
		for _, k := range keys {
			pairs = append(pairs, k+"="+params[k])
		}
		attrs = append(attrs, "params", strings.Join(pairs, ","))
	}
	attrs = append(attrs, "outcome", outcome)
	logger.Info("audit", attrs...)
}

// AuditErr is Audit for failed calls: the outcome is "denied"/"error" plus
// the error text.
func AuditErr(logger *slog.Logger, actorCN, method string, params map[string]string, outcome string, err error) {
	if logger == nil {
		return
	}
	if err != nil {
		outcome = outcome + ": " + err.Error()
	}
	Audit(logger, actorCN, method, params, outcome)
}
