// Package client talks to Stratum's agent API. Every call carries a deadline
// and retries transient failures; callers decide what an error means, and the
// CLI never lets one fail a deploy.
package client

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/stratum-dev/agent/gen/stratum/agent/v1"
	"github.com/stratum-dev/agent/gen/stratum/agent/v1/agentv1connect"
	"github.com/stratum-dev/agent/snapshot"
)

// Client is an authenticated connection to Stratum.
type Client struct {
	Capture agentv1connect.CaptureServiceClient
	Check   agentv1connect.CheckServiceClient
}

// Options configures a Client.
type Options struct {
	// BaseURL is Stratum's address, e.g. https://stratum.example.com.
	BaseURL string
	// Token is sent as a bearer token: the capture token, or the check's
	// GitHub Actions OIDC token.
	Token string
	// Project names the project ("org/slug") for Stratum's local dev mode,
	// where one fixed token serves every project.
	Project string
}

// ProjectHeader carries Options.Project.
const ProjectHeader = "Stratum-Project"

// New returns a client for Stratum's agent API, served under /ingest.
func New(opts Options) *Client {
	httpClient := &http.Client{Timeout: 2 * time.Minute}
	interceptors := connect.WithInterceptors(authInterceptor(opts.Token, opts.Project), retryInterceptor())
	base := opts.BaseURL + "/ingest"
	return &Client{
		Capture: agentv1connect.NewCaptureServiceClient(httpClient, base, interceptors),
		Check:   agentv1connect.NewCheckServiceClient(httpClient, base, interceptors),
	}
}

func authInterceptor(token, project string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			if project != "" {
				req.Header().Set(ProjectHeader, project)
			}
			return next(ctx, req)
		}
	}
}

// retryInterceptor retries a call that failed for reasons a retry can fix,
// waiting 1, 2 and 4 seconds. Uploads are safe to retry: they carry an ID.
func retryInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			wait := time.Second
			for attempt := 1; ; attempt++ {
				callCtx, cancel := context.WithTimeout(ctx, time.Minute)
				resp, err := next(callCtx, req)
				cancel()
				if err == nil || attempt == 4 || !retryable(err) {
					return resp, err
				}
				select {
				case <-ctx.Done():
					return nil, errors.Join(err, ctx.Err())
				case <-time.After(wait):
				}
				wait *= 2
			}
		}
	}
}

func retryable(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeResourceExhausted, connect.CodeUnknown, connect.CodeInternal:
		return true
	}
	return false
}

// Tool converts the API's migration tool to the snapshot's.
func Tool(t agentv1.MigrationTool) snapshot.Tool {
	switch t {
	case agentv1.MigrationTool_MIGRATION_TOOL_ATLAS:
		return snapshot.ToolAtlas
	case agentv1.MigrationTool_MIGRATION_TOOL_FLYWAY:
		return snapshot.ToolFlyway
	}
	return ""
}
