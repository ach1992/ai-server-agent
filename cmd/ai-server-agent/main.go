package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpserver "github.com/ach1992/ai-server-agent/internal/mcp"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "job-runner" {
		os.Exit(executor.RunJobHelper(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "workspace-helper" {
		os.Exit(executor.RunWorkspaceFileHelper())
	}

	cfgPath := flag.String("config", "/etc/ai-server-agent/config.json", "config path")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fatal("usage: ai-server-agent [serve|executor|print-config|validate-config|runtime-settings]")
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
		for _, spec := range config.RuntimeSettingSpecs() {
			fmt.Printf("%s=%d %s (default=%d, range=%d..%d, restart=agent+executor) - %s\n", spec.Key, values[spec.Key], spec.Unit, spec.Default, spec.Minimum, spec.Maximum, spec.Description)
		}
	default:
		fatal("unknown command")
	}
}

func fatal(s string) {
	fmt.Fprintln(os.Stderr, s)
	os.Exit(2)
}
