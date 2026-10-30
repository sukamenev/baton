package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const version = "0.3.6"

func isKnownAgent(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "claude", "codex", "agy", "pi", "hermes", "all", "auto":
		return true
	}
	return false
}

// resolveAgentAndQuery allows natural one-argument calls:
// - `baton load`                   -> agent="all",    query="latest"
// - `baton load my_session`        -> agent="all",    query="my_session"
// - `baton load 1`                 -> agent="all",    query="1"
// - `baton load claude`            -> agent="claude", query="latest"
// - `baton load claude my_session` -> agent="claude", query="my_session"
func resolveAgentAndQuery(args []string, defaultQuery string) (string, string) {
	if len(args) == 0 {
		return "all", defaultQuery
	}
	first := strings.TrimSpace(args[0])
	if len(args) == 1 {
		if isKnownAgent(first) {
			return strings.ToLower(first), defaultQuery
		}
		return "all", first
	}
	if isKnownAgent(first) {
		return strings.ToLower(first), strings.Join(args[1:], " ")
	}
	return "all", strings.Join(args, " ")
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		return
	}

	cmd := os.Args[1]
	if cmd == "update" && len(os.Args) > 2 && (os.Args[2] == "--check-quiet" || os.Args[2] == "--quiet") {
		runQuietCheckAndExit()
		return
	}

	// Non-blocking once-a-day version check against latest.txt
	if cmd != "update" && cmd != "upgrade" {
		maybeTriggerQuietUpdate()
	}

	switch cmd {
	case "install", "setup":
		if err := runInstall(); err != nil {
			fmt.Fprintf(os.Stderr, "Install error: %v\n", err)
			os.Exit(1)
		}
	case "update", "upgrade":
		report, err := runSelfUpdate()
		fmt.Print(report)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Update error: %v\n", err)
			os.Exit(1)
		}
	case "status":
		runStatus()
	case "store", "save", "name":
		if len(os.Args) > 2 && (os.Args[2] == "--remote" || os.Args[2] == "-r" || os.Args[2] == "remote") {
			name := ""
			if len(os.Args) > 3 {
				name = os.Args[3]
			}
			token, err := pushRemoteSession(name, "auto", "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Store remote error: %v\n", err)
				os.Exit(1)
			}
			printRemoteTransferPrompt(token)
			return
		}
		if msg := saveArgsError(os.Args[2:]); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
			os.Exit(1)
		}
		alias := ""
		if len(os.Args) < 3 {
			cwd, _ := os.Getwd()
			alias = filepath.Base(cwd)
			if alias == "" || alias == "." || alias == "/" {
				alias = "saved-session"
			}
		} else {
			alias = os.Args[2]
		}
		msg, err := nameSession(alias)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(msg)
	case "list", "candidates":
		agent, query := resolveAgentAndQuery(os.Args[2:], "")
		fmt.Println(listSessionsReport(agent, query))
	case "import":
		report, err := importSession(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Import error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(report)
	case "rm", "delete", "remove":
		report, err := removeSavedSession(strings.Join(os.Args[2:], " "))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Remove error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(report)
	case "clear", "clean", "reset":
		msg, err := clearSavedSessions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Clear error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(msg)
	case "load", "pull", "apply":
		// If a remote URL (https://...#key) or transfer token (baton:...#key) is passed,
		// automatically download, decrypt, and restore the remote session!
		if len(os.Args) > 2 {
			arg := strings.TrimSpace(os.Args[2])
			if strings.HasPrefix(arg, "baton:") || strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://") {
				report, err := pullRemoteSession(arg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error restoring remote session: %v\n", err)
					os.Exit(1)
				}
				fmt.Println(report)
				return
			}
		}
		agent, query := resolveAgentAndQuery(os.Args[2:], "latest")
		report, err := loadSessionContext(agent, query)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(report)
	case "turn":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: baton turn <session_id> <turn_id>")
			os.Exit(1)
		}
		turnID, _ := strconv.Atoi(os.Args[3])
		text, err := getSessionTurnDetails(os.Args[2], turnID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(text)
	case "push":
		// baton push [name] [storage] [local_dir]
		var name, storage, localDir string
		if len(os.Args) > 2 {
			name = os.Args[2]
		}
		if len(os.Args) > 3 {
			storage = os.Args[3]
		}
		if len(os.Args) > 4 {
			localDir = os.Args[4]
		}
		token, err := pushRemoteSession(name, storage, localDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Push error: %v\n", err)
			os.Exit(1)
		}
		printRemoteTransferPrompt(token)
	case "version", "--version", "-v":
		fmt.Printf("baton %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		printUsage()
		os.Exit(1)
	}
}

// saveArgsError rejects the pre-0.3.6 forms `baton save <name> <#|agent>` and
// `baton save <#>`: save works only with the current session now.
func saveArgsError(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if len(args) == 1 {
		if _, err := strconv.Atoi(strings.TrimPrefix(args[0], "#")); err != nil {
			return ""
		}
		return fmt.Sprintf("Error: baton save saves only the current session. To save session %s from the list, run:\n  baton import %s\n(run 'baton import' to see the list)", args[0], args[0])
	}
	return fmt.Sprintf("Error: baton save takes only a name and saves the current session. To save another session under a name, run:\n  baton import <#|id> as %s\n(run 'baton import' to see the list)", args[0])
}

func printRemoteTransferPrompt(token string) {
	fmt.Printf(`Encrypted session token:
  %s

To continue in another agent or on a machine where baton is not installed yet, paste this prompt:
  Load session %s using baton (if baton is not installed, install it from https://github.com/sukamenev/baton following INSTALL.md)
or if baton is already installed, run:
  /baton load %s
`, token, token, token)
}

func printUsage() {
	fmt.Printf(`baton %s — Zero-dependency CLI & Skill for lossless session handoff across AI coding agents (claude, codex, agy, pi, hermes)

Usage:
  baton save [name]                 Save the current session into baton's local store (defaults to folder name)
  baton push [name]                 Pack (tar.gz), encrypt (AES-256-GCM), and upload the current session + uncommitted files
  baton load [name|#|id|URL|token]  Load the latest saved session in baton, a specific saved session, or a remote URL/token
  baton list                        List saved sessions in baton's local store (#1, #2, ...)
  baton import [#|name|id] [as <name>] [--no-load]
                                    List raw sessions of all agents, or import one into baton's store and load it
  baton rm <name|#>                 Delete one saved session from baton's store (# as shown by baton list)
  baton clear                       Delete all locally saved sessions and temporary handoff files in baton
  baton turn <session_id> <turn_id> Print full untruncated output of a specific conversation turn
  baton install                     Install SKILL.md and /baton slash command across all supported agents
  baton status                      Check skill installation status for all supported agents
  baton update                      Update baton (via go install or SHA-256 verified OS release) and refresh SKILL.md
`, version)
}
