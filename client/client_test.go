package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	agentv1 "github.com/dbproof/agent/gen/dbproof/agent/v1"
	"github.com/dbproof/agent/gen/dbproof/agent/v1/agentv1connect"
)

// verdicts answers GetCheckVerdict with the errors in fail, one per call,
// then with a ready verdict.
type verdicts struct {
	agentv1connect.UnimplementedCheckServiceHandler
	fail  []error
	calls int
	auth  []string
}

func (v *verdicts) GetCheckVerdict(ctx context.Context, _ *agentv1.GetCheckVerdictRequest) (*agentv1.GetCheckVerdictResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	v.auth = append(v.auth, info.RequestHeader().Get("Authorization"))
	v.calls++
	if v.calls <= len(v.fail) {
		return nil, v.fail[v.calls-1]
	}
	return &agentv1.GetCheckVerdictResponse{Ready: true}, nil
}

func serve(t *testing.T, v *verdicts) *Client {
	t.Helper()
	server := connect.NewServer()
	agentv1connect.RegisterCheckServiceHandler(server, v)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(http.StripPrefix("/ingest", mux))
	t.Cleanup(srv.Close)
	return New(Options{BaseURL: srv.URL, Token: "t0ken"})
}

func TestRetriesTransientFailures(t *testing.T) {
	v := &verdicts{fail: []error{connect.NewError(connect.CodeUnavailable, "restarting")}}
	resp, err := serve(t, v).Check.GetCheckVerdict(t.Context(), &agentv1.GetCheckVerdictRequest{CheckId: "c1"})
	if err != nil {
		t.Fatalf("GetCheckVerdict: %v", err)
	}
	if !resp.GetReady() {
		t.Error("got the failed attempt's response, want the retry's")
	}
	if v.calls != 2 {
		t.Errorf("server saw %d calls, want 2", v.calls)
	}
	for i, a := range v.auth {
		if a != "Bearer t0ken" {
			t.Errorf("call %d sent Authorization %q", i+1, a)
		}
	}
}

func TestDoesNotRetryRejections(t *testing.T) {
	v := &verdicts{fail: []error{connect.NewError(connect.CodePermissionDenied, "wrong repository")}}
	_, err := serve(t, v).Check.GetCheckVerdict(t.Context(), &agentv1.GetCheckVerdictRequest{CheckId: "c1"})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("err = %v, want PermissionDenied", err)
	}
	if v.calls != 1 {
		t.Errorf("server saw %d calls, want 1", v.calls)
	}
}
