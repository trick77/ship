package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/trick77/ship/kit/logging"
)

// ServerError logs err and answers 500 with msg, so the cause stays in the log and the client gets a fixed phrase.
// Every 500 a handler sends goes through it, so none is silent. The line is "request failed" at error, with
// client_message and err; it carries the request id (AccessLog binds it to r's context), which joins it to the
// access line, so it logs no path: that line has the masked one.
//
// A request whose client went away fails on whatever it was waiting for, with an error that hides the cancel (a
// database driver's interrupt reads like corruption). That one is "request cancelled" at warn, with the cancel's
// cause next to err: the client left, nothing broke. A deadline the server set (a timeout middleware) is no client
// leaving: it stays "request failed" at error. Serve's Close after a drain overrun cancels the same way a client
// does and logs at warn too; Serve's own error reports that overrun.
//
// The line goes to slog.Default, which logging.Setup installs. err is logged as it is: a cause that may embed a URL
// with a token in its query is the caller's to redact first.
func ServerError(w http.ResponseWriter, r *http.Request, err error, msg string) {
	ctx := r.Context()
	if errors.Is(ctx.Err(), context.Canceled) {
		slog.WarnContext(ctx, "request cancelled", "cause", context.Cause(ctx).Error(), "client_message", msg,
			logging.Err(err))
	} else {
		slog.ErrorContext(ctx, "request failed", "client_message", msg, logging.Err(err))
	}
	WriteError(w, http.StatusInternalServerError, msg)
}
