package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"time"

	"github.com/containerd/containerd/log"

	types "github.com/interlink-hq/interlink/pkg/interlink"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

// EstimateHandler handles HTTP POST requests asking when the remote system would start a
// job of a given shape if it were submitted now, so that the caller can hold the job back
// while the start is far. The request is forwarded as it came to the plugin's /estimate,
// and the plugin's answer is relayed. It is meant for plugins that list "estimate" in
// their ping answer.
//
// Request body: JSON-encoded EstimateRequest
// Response: JSON-encoded EstimateResponse
//
// HTTP Status Codes:
//   - 200: The plugin answered with a start time
//   - 400: Bad request (malformed body or fewer than one node)
//   - 500: Internal server error (sidecar communication failures)
//   - any other: the plugin's own code, with its error in the body (for instance a limit of
//     the remote system that refuses such a job now)
func (h *InterLinkHandler) EstimateHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	_, span := tracer.Start(h.Ctx, "EstimateAPI", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))
	defer span.End()
	defer types.SetDurationSpan(start, span)
	defer types.SetInfoFromHeaders(span, &r.Header)

	log.G(h.Ctx).Info("InterLink: received Estimate call")

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.G(h.Ctx).Error(err)
		return
	}

	var request types.EstimateRequest
	if err := json.Unmarshal(bodyBytes, &request); err != nil {
		writeEstimateError(h.Ctx, w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if request.Nodes < 1 {
		writeEstimateError(h.Ctx, w, http.StatusBadRequest, fmt.Errorf("nodes is %d, want at least 1", request.Nodes))
		return
	}

	span.SetAttributes(
		attribute.Int("estimate.nodes", request.Nodes),
		attribute.Int("estimate.gpusPerNode", request.GPUsPerNode),
	)

	// The body goes on unchanged, so that fields the plugin knows and interLink does not
	// still reach it.
	req, err := http.NewRequest(http.MethodPost, h.SidecarEndpoint+"/estimate", bytes.NewReader(bodyBytes))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.G(h.Ctx).Error(err)
		return
	}

	log.G(h.Ctx).Infof("InterLink: forwarding Estimate call to sidecar (%d node(s), %d GPU(s) each)", request.Nodes, request.GPUsPerNode)

	sessionContext := GetSessionContext(r)
	ForwardAccessToken(r, req)
	_, err = ReqWithError(h.Ctx, req, w, start, span, true, false, sessionContext, h.ClientHTTP)
	if err != nil {
		log.L.Error(err)
		return
	}
}

// writeEstimateError answers the caller with statusCode and the error text, escaped as
// ReqWithError escapes the errors of the sidecar.
func writeEstimateError(ctx context.Context, w http.ResponseWriter, statusCode int, err error) {
	log.G(ctx).Error(err)
	w.WriteHeader(statusCode)
	if _, werr := w.Write([]byte(html.EscapeString(err.Error()))); werr != nil {
		log.G(ctx).Error(werr)
	}
}
