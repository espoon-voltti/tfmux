// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/espoon-voltti/tfmux/internal/config"
)

func TestRequireBearer(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(requireBearer("secret", ok))
	defer srv.Close()
	for _, c := range []struct {
		header string
		want   int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"Bearer secretx", http.StatusUnauthorized},
		{"Bearer secret", http.StatusNoContent},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("header %q: status %d, want %d", c.header, res.StatusCode, c.want)
		}
	}
}

func TestParseMCPFlags(t *testing.T) {
	cfg := config.Default()
	f, err := parseMCPFlags(nil, cfg)
	if err != nil || f.all || f.listen != "127.0.0.1:7411" || f.noAuth || f.stdio {
		t.Errorf("defaults: %+v err %v", f, err)
	}
	cfg.MCP.Tools = config.MCPToolsAll
	f, err = parseMCPFlags([]string{"--listen", "127.0.0.1:9", "--no-auth", "--stdio"}, cfg)
	if err != nil || !f.all || f.listen != "127.0.0.1:9" || !f.noAuth || !f.stdio {
		t.Errorf("flags: %+v err %v", f, err)
	}
	if _, err := parseMCPFlags([]string{"--bogus"}, cfg); err == nil {
		t.Error("unknown flag accepted")
	}
}

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:7411": true, "localhost:1": true, "[::1]:1": true, "0.0.0.0:1": false, "10.0.0.1:1": false, "nonsense": false} {
		if loopback(addr) != want {
			t.Errorf("loopback(%q) = %v", addr, !want)
		}
	}
}

func TestPrintMCPConfig(t *testing.T) {
	var out strings.Builder
	printMCPConfig(&out, "127.0.0.1:7411", "tok")
	s := out.String()
	if !strings.Contains(s, "claude mcp add -s user --transport http tfmux http://127.0.0.1:7411/mcp --header \"Authorization: Bearer tok\"") {
		t.Errorf("output:\n%s", s)
	}
	if !strings.Contains(s, `"url": "http://127.0.0.1:7411/mcp"`) {
		t.Errorf("json snippet missing:\n%s", s)
	}
}
