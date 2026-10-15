package mcp

import (
	"context"

	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type CodeInspectInput struct {
	Workspace   string `json:"workspace" jsonschema:"Exact absolute resolved aiworker workspace/worktree directory"`
	Path        string `json:"path" jsonschema:"Workspace-relative regular .go .ts .tsx .mts .cts .js .jsx .mjs or .cjs .py .pyi .sh .bash .htm .html .css .sql .pgsql .psql .mysql or .mariadb .rs or .php source path; no symlinks or .git files"`
	Language    string `json:"language,omitempty" jsonschema:"Optional language go, javascript, typescript, python, shell, html, css, sql, rust or php; checked against path extension (never selects project commands)"`
	FileVersion string `json:"file_version" jsonschema:"Required exact file_version returned by worker workspace_stat/read. Semantic results fail when source changes."`
	SQLDialect  string `json:"sql_dialect,omitempty" jsonschema:"Required for SQL files: explicit postgres, mysql or mariadb (MariaDB uses MySQL syntax subset); no implicit connections or project dialect fallback"`
	Line        int    `json:"line,omitempty" jsonschema:"Zero-based source line for definition/references"`
	Character   int    `json:"character,omitempty" jsonschema:"Zero-based UTF-16 character index for definition/references"`
}

func (s *Server) registerCodeTools() {
	tools := []struct{ suffix, description string }{
		{"definition", "Resolve a source symbol definition using the trusted Go, JS/TS, Python, Shell, HTML/CSS, SQL, Rust or PHP LSP profile and exact aiworker file_version"},
		{"references", "Retrieve bounded source references through trusted Go, JS/TS, Python, Shell, HTML/CSS, SQL, Rust or PHP LSP; output never mutates files"},
		{"symbols", "Retrieve bounded Go, JavaScript/TypeScript, Python, Shell, HTML/CSS, SQL, Rust or PHP document symbols via real language semantics"},
		{"diagnostics", "Retrieve bounded Go, JavaScript/TypeScript, Python, Shell, HTML/CSS, SQL, Rust or PHP LSP diagnostics, negotiated as push or pull"},
	}
	for _, tool := range tools {
		method := tool.suffix
		mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
			Name: "code_" + method, Description: tool.description + ". Optional pinned local adapters; method support varies by language. SQL requires explicit dialect and never opens a database. Requires exact file_version; no implicit installs or project scripts; unsupported methods fail closed.",
			Annotations: annotations(true, false, true, false),
		}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input CodeInspectInput) (*mcpsdk.CallToolResult, executor.Response, error) {
			resp, err := executor.ClientCallContext(ctx, s.cfg.ExecutorSocket, s.executorToken, executor.Request{
				Action: "code_inspect", CodeMethod: method, Workspace: input.Workspace, Path: input.Path,
				FileVersion: input.FileVersion, Language: input.Language, SQLDialect: input.SQLDialect, Line: input.Line, Character: input.Character,
			})
			if err != nil {
				return executorTransportErrorResult(err)
			}
			return s.responseResult(resp)
		})
	}
}
