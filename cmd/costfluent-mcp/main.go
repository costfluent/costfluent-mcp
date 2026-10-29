// Command costfluent-mcp serves Costfluent cost data to an AI assistant over the Model Context
// Protocol on stdin and stdout, with the read tools its API token's capabilities allow.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/costfluent/costfluent-mcp/internal/config"
	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/server"
)

// version is set at release by -ldflags "-X main.version=...".
var version = "dev"

// apiAttemptTimeout bounds each attempt at an API call; a variable only so tests can shorten it.
var apiAttemptTimeout = server.APIAttemptTimeout

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// stdout carries the protocol, so everything the server says for itself goes to stderr, which
	// MCP clients keep as the server's log.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(ctx, os.Getenv, logger, &mcp.StdioTransport{}); err != nil {
		logger.Error("costfluent-mcp stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, logger *slog.Logger, transport mcp.Transport) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	client := costfluent.NewClient(
		costfluent.WithAPIKey(cfg.APIToken),
		costfluent.WithBaseURL(cfg.BaseURL),
		costfluent.WithUserAgent("costfluent-mcp/"+version),
		costfluent.WithTimeout(apiAttemptTimeout),
	)
	srv, me, err := server.New(ctx, client, version)
	if err != nil {
		return err
	}
	workspace := ""
	if me.WorkspaceID != nil {
		workspace = *me.WorkspaceID
	}
	logger.Info("serving",
		"version", version, "api", cfg.BaseURL, "token_name", me.TokenName, "token_type", me.TokenType,
		"workspace", workspace, "tools", strings.Join(server.ToolNames(me), ","))
	// A client closing stdin and a signal both end the session; neither is a failure.
	if err := srv.Run(ctx, transport); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
