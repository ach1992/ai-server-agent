package main

import (
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
	cfgPath := flag.String("config", "/etc/ai-server-agent/config.json", "config path")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fatal("usage: ai-server-agent [serve|executor|print-config]")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err.Error())
	}
	switch args[0] {
	case "serve":
		if err := mcpserver.Serve(cfg); err != nil {
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
	default:
		fatal("unknown command")
	}
}
func fatal(s string) { fmt.Fprintln(os.Stderr, s); os.Exit(2) }
