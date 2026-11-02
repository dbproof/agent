// Package client talks to DbProof's agent API. Every call carries a deadline
// and retries transient failures; callers decide what an error means, and the
// CLI never lets one fail a deploy.
package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	agentv1 "github.com/dbproof/agent/gen/dbproof/agent/v1"
	"github.com/dbproof/agent/gen/dbproof/agent/v1/agentv1connect"
	"github.com/dbproof/agent/snapshot"
)

// Client is an authenticated connection to DbProof.
type Client struct {
	Capture agentv1connect.CaptureServiceClient
	Check   agentv1connect.CheckServiceClient
}

// Options configures a Client.
type Options struct {
	// BaseURL is DbProof's address, e.g. https://dbproof.example.com.
	BaseURL string
	// Token is sent as a bearer token: the capture token, or the check's
	// GitHub Actions OIDC token.
	Token string
	// Project names the project ("org/slug") for DbProof's local dev mode,
	// where one fixed token serves every project.
	Project string
}

// ProjectHeader carries Options.Project.
const ProjectHeader = "DbProof-Project"

// New returns a client for DbProof's agent API, served under /ingest.
func New(opts Options) *Client {
	// BeginCheck's response carries the whole snapshot, which can pass v2's
	// default 4 MiB limit.
	transport := connecthttp.NewTransport(&http.Client{Timeout: 2 * time.Minute}, opts.BaseURL+"/ingest",
		connecthttp.WithReadMaxBytes(0))
	c := connect.NewClient(transport, authInterceptor(opts.Token, opts.Project), retryInterceptor)
	return &Client{
		Capture: agentv1connect.NewCaptureServiceClient(c),
		Check:   agentv1connect.NewCheckServiceClient(c),
	}
}

func authInterceptor(token, project string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			info, _ := connect.CallInfoForClientContext(ctx)
			info.RequestHeader().Set("Authorization", "Bearer "+token)
			if project != "" {
				info.RequestHeader().Set(ProjectHeader, project)
			}
			return next(ctx, spec)
		}
	}
}

// retryInterceptor retries a unary call that failed for reasons a retry can
// fix, waiting 1, 2 and 4 seconds. Uploads are safe to retry: they carry an
// ID. A unary call's response arrives on the stream, after the interceptor
// chain has returned, so the call runs on the stream's Receive.
func retryInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		if spec.StreamType != connect.StreamTypeUnary {
			return next(ctx, spec)
		}
		return &retryStream{call: func(req, res any) error {
			return retry(ctx, func(ctx context.Context) error { return callUnary(ctx, next, spec, req, res) })
		}}, nil
	}
}

func retry(ctx context.Context, call func(context.Context) error) error {
	wait := time.Second
	for attempt := 1; ; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := call(callCtx)
		cancel()
		if err == nil || attempt == 4 || !retryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func callUnary(ctx context.Context, next connect.ClientFunc, spec connect.Spec, req, res any) (err error) {
	stream, err := next(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, stream.Close()) }()
	if err := stream.Send(req); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	return stream.Receive(res)
}

// retryStream holds a unary call's request until Receive, then makes the
// call.
type retryStream struct {
	call func(req, res any) error
	req  any
}

func (s *retryStream) Send(msg any) error {
	s.req = msg
	return nil
}

func (s *retryStream) Receive(res any) error {
	if s.req == nil {
		return io.EOF
	}
	err := s.call(s.req, res)
	s.req = nil
	return err
}

func (s *retryStream) Close() error       { return nil }
func (s *retryStream) CloseSend() error   { return nil }
func (s *retryStream) SendHeaders() error { return nil }

func retryable(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeResourceExhausted, connect.CodeUnknown, connect.CodeInternal:
		return true
	default:
		return false
	}
}

// Tool converts the API's migration tool to the snapshot's.
func Tool(t agentv1.MigrationTool) snapshot.Tool {
	switch t {
	case agentv1.MigrationTool_MIGRATION_TOOL_ATLAS:
		return snapshot.ToolAtlas
	case agentv1.MigrationTool_MIGRATION_TOOL_FLYWAY:
		return snapshot.ToolFlyway
	case agentv1.MigrationTool_MIGRATION_TOOL_PRISMA:
		return snapshot.ToolPrisma
	case agentv1.MigrationTool_MIGRATION_TOOL_DRIZZLE:
		return snapshot.ToolDrizzle
	default:
		return ""
	}
}
