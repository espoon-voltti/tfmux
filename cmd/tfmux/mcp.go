// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/espoon-voltti/tfmux/internal/config"
	"github.com/espoon-voltti/tfmux/internal/mcpserver"
)

// mcpPath is where the Streamable HTTP endpoint is mounted.
const mcpPath = "/mcp"

type mcpFlags struct {
	all         bool
	listen      string
	noAuth      bool
	stdio       bool
	printConfig bool
}

func parseMCPFlags(args []string, cfg *config.Config) (mcpFlags, error) {
	f := mcpFlags{all: cfg.MCP.AllTools(), listen: cfg.MCP.Listen}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--all":
			f.all = true
		case "--no-auth":
			f.noAuth = true
		case "--stdio":
			f.stdio = true
		case "--print-config":
			f.printConfig = true
		case "--listen":
			if i+1 >= len(args) {
				return f, errors.New("mcp: --listen needs an address")
			}
			i++
			f.listen = args[i]
		default:
			if strings.HasPrefix(a, "--listen=") {
				f.listen = strings.TrimPrefix(a, "--listen=")
				continue
			}
			return f, fmt.Errorf("mcp: unknown flag %q", a)
		}
	}
	return f, nil
}

func runMCP(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	f, err := parseMCPFlags(args, cfg)
	if err != nil {
		return err
	}
	store, _, err := openStore()
	if err != nil {
		return err
	}
	if f.printConfig {
		token, err := store.MCPToken()
		if err != nil {
			return err
		}
		printMCPConfig(os.Stdout, f.listen, token)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := log.New(os.Stderr, "tfmux mcp: ", 0)
	srv, err := mcpserver.New(ctx, mcpserver.Options{Cfg: cfg, Store: store, AllTools: f.all, Version: version, Log: logger})
	if err != nil {
		return err
	}
	defer srv.Close()

	if f.stdio {
		logger.Printf("serving over stdio (%s tools)", toolSetName(f.all))
		return srv.MCP().Run(ctx, &mcp.StdioTransport{})
	}

	handler := http.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	if f.noAuth {
		if !loopback(f.listen) {
			return fmt.Errorf("mcp: --no-auth is only allowed on a loopback address, not %s", f.listen)
		}
		logger.Printf("WARNING: no authentication; any local process can call these tools")
	} else {
		token, err := store.MCPToken()
		if err != nil {
			return err
		}
		handler = requireBearer(token, handler)
	}
	mux := http.NewServeMux()
	mux.Handle(mcpPath, handler)
	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	logger.Printf("listening on http://%s%s (%s tools); `tfmux mcp --print-config` prints the client setup", ln.Addr(), mcpPath, toolSetName(f.all))
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func toolSetName(all bool) string {
	if all {
		return "all"
	}
	return "read-only"
}

// loopback reports whether addr binds only a loopback interface.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireBearer rejects requests whose Authorization header does not carry
// token. The comparison is constant-time.
func requireBearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tfmux"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// printMCPConfig prints how to point a client at this server.
func printMCPConfig(out io.Writer, listen, token string) {
	url := "http://" + listen + mcpPath
	fmt.Fprintf(out, `# Run once, outside the sandbox, to register tfmux for Claude Code
# (the token is then stored in ~/.claude.json, which a sandbox may read):
claude mcp add -s user --transport http tfmux %s --header "Authorization: Bearer %s"

# Or in a .mcp.json / settings "mcpServers" block:
{
  "tfmux": {
    "type": "http",
    "url": %q,
    "headers": { "Authorization": "Bearer %s" }
  }
}
`, url, token, url, token)
}
