package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"elephant/internal/agent"
	"elephant/internal/model"
)

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// runSetup registers Elephant's MCP server with one coding agent. It runs
// before the database is opened so setup never depends on usable storage.
func runSetup(ctx context.Context, out, logs io.Writer, args []string, dbPath string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(logs)
	configPath := fs.String("config", "", "write this configuration file instead of the agent default")
	actor := fs.String("actor", "", "explicit actor identity recorded on new entries")
	hooks := fs.Bool("hooks", false, "also install a session-start hook that runs recall --brief")
	structured := fs.Bool("json", false, "print machine-readable output")
	if len(args) == 0 {
		return fmt.Errorf("%w: setup requires exactly one agent name (%s)", model.ErrInvalidInput, strings.Join(agent.Names(), ", "))
	}
	name, rest := args[0], args[1:]
	if err := parseFlags(fs, rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: setup requires exactly one agent name (%s)", model.ErrInvalidInput, strings.Join(agent.Names(), ", "))
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: locate the running executable: %v", model.ErrInvalidInput, err)
	}
	result, err := agent.Configure(agent.Input{Agent: name, Executable: executable, Database: dbPath, ActorID: *actor, ConfigPath: *configPath, Hooks: *hooks}, nil)
	if err != nil {
		return err
	}
	if *structured {
		return writeJSON(out, result)
	}
	if result.Manual != "" {
		fmt.Fprintln(out, result.Manual)
		return nil
	}
	fmt.Fprintf(out, "configured %s at %s\n", result.Agent, result.ConfigPath)
	fmt.Fprintf(out, "MCP command: %s\n", strings.Join(result.Command, " "))
	fmt.Fprintf(out, "actor: %s\n", result.ActorID)
	if result.Changed {
		fmt.Fprintln(out, "wrote the Elephant server entry; unrelated settings were preserved")
	} else {
		fmt.Fprintln(out, "already up to date; no changes written")
	}
	if result.HookPath != "" {
		fmt.Fprintf(out, "session hook: %s\n", result.HookPath)
		fmt.Fprintf(out, "hook command: %s\n", result.HookCommand)
		if result.HookChanged {
			fmt.Fprintln(out, "installed the session-start hook; other hooks were preserved")
		} else {
			fmt.Fprintln(out, "session-start hook already up to date")
		}
	}
	for _, note := range result.Notes {
		fmt.Fprintln(out, "note: "+note)
	}
	return nil
}
