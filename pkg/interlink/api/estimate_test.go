package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	types "github.com/interlink-hq/interlink/pkg/interlink"
)

const (
	testEstimatePath    = "/estimate"
	testEstimateSession = "Request-estimate-123"
)

// estimateCall is what the fake sidecar received in one /estimate call.
type estimateCall struct {
	path    string
	session string
	token   string
	body    string
}

// newEstimateSidecar starts a fake sidecar that records every call and answers each one
// with statusCode and answer, and returns an InterLinkHandler pointed at it.
func newEstimateSidecar(t *testing.T, statusCode int, answer string) (*InterLinkHandler, chan estimateCall) {
	t.Helper()
	calls := make(chan estimateCall, 4)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		calls <- estimateCall{path: r.URL.Path, session: r.Header.Get("InterLink-Http-Session"), token: r.Header.Get(accessTokenHeader), body: string(body)}

		w.WriteHeader(statusCode)
		if _, err := io.WriteString(w, answer); err != nil {
			panic(err)
		}
	}))
	t.Cleanup(sidecar.Close)

	handler := &InterLinkHandler{
		Ctx:             context.Background(),
		SidecarEndpoint: sidecar.URL,
		ClientHTTP:      sidecar.Client(),
	}
	return handler, calls
}

// estimateRequest returns a POST /estimate with body, in the test session.
func estimateRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, testEstimatePath, bytes.NewBufferString(body))
	r.Header.Set("InterLink-Http-Session", testEstimateSession)
	return r
}

func TestEstimateHandler_ForwardsRequest(t *testing.T) {
	_, cleanup := setupTestTracer()
	defer cleanup()

	// flavor is not an EstimateRequest field: it reaches the plugin all the same
	body := `{"nodes":2,"gpusPerNode":4,"cpusPerNode":80,"time":"01:30:00","flags":"--qos=debug","flavor":"large"}`
	answer := `{"start":"2026-10-30T14:00:00Z","raw":"Job 4242 to start at 2026-10-30T15:00:00"}`
	handler, calls := newEstimateSidecar(t, http.StatusOK, answer)

	r := estimateRequest(body)
	r.Header.Set(accessTokenHeader, testAccessToken)
	w := httptest.NewRecorder()
	handler.EstimateHandler(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp types.EstimateResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Start.Equal(time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)), "start = %s", resp.Start)

	require.Len(t, calls, 1)
	call := <-calls
	assert.Equal(t, testEstimatePath, call.path)
	assert.Equal(t, testEstimateSession, call.session)
	assert.Equal(t, testAccessToken, call.token, "the caller's token reaches the plugin")
	assert.Equal(t, body, call.body)
}

func TestEstimateHandler_RelaysSidecarErrors(t *testing.T) {
	_, cleanup := setupTestTracer()
	defer cleanup()

	tests := []struct {
		name       string
		statusCode int
		answer     string
		want       string
	}{
		{
			name:       "limit of the remote system",
			statusCode: http.StatusBadGateway,
			answer:     `no start time in sbatch --test-only output: "sbatch: error: QOSMaxSubmitJobPerUserLimit"`,
			want:       "QOSMaxSubmitJobPerUserLimit",
		},
		{
			name:       "shape refused",
			statusCode: http.StatusBadRequest,
			answer:     "time 1:00 is not HH:MM:SS",
			want:       "time 1:00 is not HH:MM:SS",
		},
		{
			name:       "plugin without /estimate",
			statusCode: http.StatusNotFound,
			answer:     "404 page not found",
			want:       "404 page not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, calls := newEstimateSidecar(t, tt.statusCode, tt.answer)

			w := httptest.NewRecorder()
			handler.EstimateHandler(w, estimateRequest(`{"nodes":1,"gpusPerNode":4}`))

			assert.Equal(t, tt.statusCode, w.Code)
			assert.Contains(t, w.Body.String(), tt.want)
			assert.Len(t, calls, 1)
		})
	}
}

func TestEstimateHandler_RefusesBadRequests(t *testing.T) {
	_, cleanup := setupTestTracer()
	defer cleanup()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "malformed body",
			body: `{"nodes":`,
		},
		{
			name: "no nodes",
			body: `{"gpusPerNode":4}`,
		},
		{
			name: "zero nodes",
			body: `{"nodes":0,"gpusPerNode":4}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, calls := newEstimateSidecar(t, http.StatusOK, "{}")

			w := httptest.NewRecorder()
			handler.EstimateHandler(w, estimateRequest(tt.body))

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.NotEmpty(t, w.Body.String())
			assert.Empty(t, calls, "the sidecar must not be called")
		})
	}
}
