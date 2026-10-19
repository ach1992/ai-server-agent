package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/credential"
	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpserver "github.com/ach1992/ai-server-agent/internal/mcp"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "job-runner" {
		os.Exit(executor.RunJobHelper(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "shell-dap-helper" {
		os.Exit(executor.RunShellDAPHelper())
	}
	if len(os.Args) > 1 && os.Args[1] == "workspace-helper" {
		os.Exit(executor.RunWorkspaceFileHelper())
	}

	cfgPath := flag.String("config", "/etc/ai-server-agent/config.json", "config path")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fatal("usage: ai-server-agent [serve|executor|print-config|validate-config|validate-credential-store|runtime-settings]")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err.Error())
	}
	switch args[0] {
	case "serve":
		if cfg.TLSConfigured() {
			err = mcpserver.ServeTLS(cfg)
		} else {
			err = mcpserver.Serve(cfg)
		}
		if err != nil {
			log.Fatal(err)
		}
	case "executor":
		b, err := os.ReadFile(cfg.ExecutorToken)
		if err != nil {
			log.Fatal(err)
		}
		s, err := executor.NewServer(cfg, strings.TrimSpace(string(b)))
		if err != nil {
			log.Fatal(err)
		}
		if err := s.Serve(); err != nil {
			log.Fatal(err)
		}
	case "print-config":
		fmt.Printf("%+v\n", cfg)
	case "validate-config":
		fmt.Println("config valid")
	case "validate-credential-store":
		// Read-only binary-specific preflight; an optional explicit path
		// validates a staged, root-owned replacement before committing it.
		if len(args) > 2 {
			fatal("validate-credential-store accepts at most one candidate store path")
		}
		path := cfg.CredentialStoreFile
		if len(args) == 2 {
			if path == "" {
				fatal("cannot override an absent named credential source")
			}
			path = args[1]
		}
		if path != "" {
			if _, err := credential.Load(path); err != nil {
				fatal("MCP credential store is incompatible with the target Agent: " + err.Error())
			}
		}
		fmt.Println("MCP credential source valid")
	case "runtime-settings":
		settings := cfg.EffectiveRuntime()
		values := make(map[string]int)
		encoded, err := json.Marshal(settings)
		if err != nil {
			fatal(err.Error())
		}
		if err := json.Unmarshal(encoded, &values); err != nil {
			fatal(err.Error())
		}
		width := runtimeSettingsOutputWidth()
		for i, spec := range config.RuntimeSettingSpecs() {
			if i > 0 {
				fmt.Println()
			}
			printRuntimeSetting(spec, values[spec.Key], width)
		}
	default:
		fatal("unknown command")
	}
}

func runtimeSettingsOutputWidth() int {
	width := 96
	if raw := strings.TrimSpace(os.Getenv("COLUMNS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			width = parsed
		}
	}
	if width < 60 {
		return 60
	}
	if width > 100 {
		return 100
	}
	return width
}

func printRuntimeSetting(spec config.RuntimeSettingSpec, value, width int) {
	// Keep the first line backward-compatible for simple operator greps while
	// moving explanatory metadata onto short, visually grouped lines.
	fmt.Printf("%s=%d %s\n", spec.Key, value, spec.Unit)
	fmt.Printf("  Default : %d %s\n", spec.Default, spec.Unit)
	fmt.Printf("  Range   : %d..%d %s\n", spec.Minimum, spec.Maximum, spec.Unit)
	fmt.Println("  Applies : Agent + Executor restart required")
	printWrappedField("  Purpose : ", spec.Description, width)
}

func printWrappedField(prefix, text string, width int) {
	words := strings.Fields(text)
	if len(words) == 0 {
		fmt.Println(prefix)
		return
	}
	if width <= len(prefix) {
		width = len(prefix) + 1
	}
	continuation := strings.Repeat(" ", len(prefix))
	line := prefix
	for _, word := range words {
		separator := ""
		if len(line) > len(prefix) {
			separator = " "
		}
		if len(line) > len(prefix) && len(line)+len(separator)+len(word) > width {
			fmt.Println(line)
			line = continuation + word
			continue
		}
		line += separator + word
	}
	fmt.Println(line)
}

func fatal(s string) {
	fmt.Fprintln(os.Stderr, s)
	os.Exit(2)
}
