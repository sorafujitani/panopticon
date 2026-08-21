package panopticon

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type stringListFlag []string

func (values *stringListFlag) String() string { return strings.Join(*values, ",") }
func (values *stringListFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type globalFlags struct {
	stateRoot string
	herdrBin  string
}

var commands = map[string]bool{
	"start": true, "dry-run": true, "status": true, "show": true, "wait": true,
	"list": true, "resume": true, "cancel": true, "cleanup": true, "doctor": true,
	"cleanup-resources": true,
}

func printJSON(value any) error {
	encoded, err := marshalIndented(value)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(encoded)
	return err
}

func positiveSeconds(value string) (float64, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, fmt.Errorf("seconds must be a positive number")
	}
	return seconds, nil
}

func intervalSeconds(value string) (float64, error) {
	seconds, err := positiveSeconds(value)
	if err != nil {
		return 0, err
	}
	if seconds < 0.2 || seconds > 30 {
		return 0, fmt.Errorf("interval must be between 0.2 and 30 seconds")
	}
	return seconds, nil
}

func newRuntime(flags globalFlags) (*RunStore, *HerdrClient, error) {
	store, err := NewRunStore(flags.stateRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("runtime initialization: %w", err)
	}
	client := NewHerdrClient(flags.herdrBin, nil)
	return store, client, nil
}

func currentRepo(value string) (string, error) {
	if value == "" {
		var err error
		value, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("current directory: %w", err)
		}
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("repo path: %w", err)
	}
	cleaned := canonicalPath(absolute)
	return cleaned, nil
}

func packageRoot() string {
	if value := os.Getenv("PANOPTICON_ROOT"); value != "" {
		return value
	}
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(executable), filepath.Dir(filepath.Dir(executable)))
	}
	for _, candidate := range candidates {
		current, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		for {
			if _, err := os.Stat(filepath.Join(current, "workflows", "standard.toml")); err == nil {
				return current
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	return ""
}

func loadPlanInputs(repoValue, workflowValue string, verifyValues []string) (string, Workflow, [][]string, error) {
	repo, err := currentRepo(repoValue)
	if err != nil {
		return "", Workflow{}, nil, err
	}
	config, err := LoadRepoConfig(repo)
	if err != nil {
		return "", Workflow{}, nil, err
	}
	path, err := ResolveWorkflowPath(repo, workflowValue, packageRoot(), config)
	if err != nil {
		return "", Workflow{}, nil, err
	}
	workflow, err := LoadWorkflow(path)
	if err != nil {
		return "", Workflow{}, nil, err
	}
	verify, err := ResolveVerifyCommands(verifyValues, config, workflow)
	if err != nil {
		return "", Workflow{}, nil, err
	}
	return repo, workflow, verify, nil
}

func addCommonFlags(fs *flag.FlagSet, global globalFlags) (*string, *string) {
	stateRoot := fs.String("state-root", global.stateRoot, "run state directory")
	herdrBin := fs.String("herdr-bin", global.herdrBin, "path to the Herdr executable")
	return stateRoot, herdrBin
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseInterleaved parses flags while allowing positional arguments to appear
// before, between, and after them, matching Python argparse. The standard
// flag package stops parsing at the first non-flag argument, so flags placed
// after a positional would silently end up in the remaining args.
func parseInterleaved(fs *flag.FlagSet, arguments []string) ([]string, error) {
	var positional []string
	current := arguments
	for len(current) > 0 {
		if current[0] == "--" {
			// Everything after "--" is positional verbatim.
			positional = append(positional, current[1:]...)
			return positional, nil
		}
		if err := fs.Parse(current); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		current = rest[1:]
	}
	return positional, nil
}

func taskValue(option, positional string) string {
	if strings.TrimSpace(option) != "" {
		return strings.TrimSpace(option)
	}
	if strings.TrimSpace(positional) != "" {
		return strings.TrimSpace(positional)
	}
	return "Investigate, implement, and verify the request"
}

func parseGlobalPrefix(arguments []string) (globalFlags, string, []string, error) {
	global := globalFlags{}
	commandIndex := -1
	for index, argument := range arguments {
		if commands[argument] {
			commandIndex = index
			break
		}
	}
	if commandIndex < 0 {
		return global, "", nil, fmt.Errorf("a command is required")
	}
	for index := 0; index < commandIndex; index++ {
		switch arguments[index] {
		case "--state-root":
			if index+1 >= commandIndex {
				return global, "", nil, fmt.Errorf("--state-root requires a value")
			}
			global.stateRoot = arguments[index+1]
			index++
		case "--herdr-bin":
			if index+1 >= commandIndex {
				return global, "", nil, fmt.Errorf("--herdr-bin requires a value")
			}
			global.herdrBin = arguments[index+1]
			index++
		default:
			return global, "", nil, fmt.Errorf("unknown option: %s", arguments[index])
		}
	}
	return global, arguments[commandIndex], arguments[commandIndex+1:], nil
}

func requireManaged(client *HerdrClient) error {
	if err := RequireHerdrEnv(client.Environ); err != nil {
		return err
	}
	return nil
}

func stateExitCode(value map[string]any) int {
	switch stringValue(value["status"]) {
	case "failed":
		return 1
	case "blocked", "cancel_requested":
		return 2
	case "cancelled":
		return 3
	default:
		return 0
	}
}

func waitExitCode(state map[string]any) int {
	switch stringValue(state["status"]) {
	case "completed":
		return 0
	case "blocked":
		return 2
	case "failed", "cancelled":
		return 1
	default:
		return 124
	}
}

func pickRunID(store *RunStore, positional, option string) (string, error) {
	if option != "" {
		return option, nil
	}
	if positional != "" {
		return positional, nil
	}
	entries, err := store.List()
	if err != nil {
		return "", fmt.Errorf("run list: %w", err)
	}
	if len(entries) == 0 {
		return "", flowError("no runs found")
	}
	runID := stringValue(entries[0]["run_id"])
	return runID, nil
}

//nolint:go-bare-error -- CLI handlers return errors to Main, which serializes them as JSON.
func runStart(arguments []string, global globalFlags) (int, error) {
	fs := newFlagSet("start")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	repo := fs.String("repo", "", "repository")
	workflowValue := fs.String("workflow", "", "workflow name or TOML path")
	var verify stringListFlag
	fs.Var(&verify, "verify", "verification argv (repeatable)")
	noWorktree := fs.Bool("no-worktree", false, "explicitly do not create a dedicated worktree")
	worktreePath := fs.String("worktree-path", "", "dedicated worktree path")
	branch := fs.String("branch", "", "worktree branch")
	base := fs.String("base", "", "worktree base ref")
	foreground := fs.Bool("foreground", false, "run to completion in this process")
	background := fs.Bool("background", true, "run in an orchestrator pane")
	taskOption := fs.String("task", "", "task")
	positionalArgs, err := parseInterleaved(fs, arguments)
	if err != nil {
		return 2, err
	}
	if *foreground {
		*background = false
	}
	positional := strings.Join(positionalArgs, " ")
	repoPath, workflow, verifyCommands, err := loadPlanInputs(*repo, *workflowValue, verify)
	if err != nil {
		return 1, err
	}
	store, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	if err := requireManaged(client); err != nil {
		return 1, err
	}
	options := StartOptions{Repo: repoPath, Workflow: workflow, Task: taskValue(*taskOption, positional), VerifyCommands: verifyCommands, UseWorktree: !*noWorktree, WorktreePath: *worktreePath, Branch: *branch, Base: *base, Background: *background, ScriptPath: executablePath()}
	state, err := NewFlowEngine(store, client).CreateRun(options)
	if err != nil {
		return 1, err
	}
	if err := printJSON(CompactState(state)); err != nil {
		return 1, err
	}
	code := stateExitCode(state)
	return code, nil
}

func runDryRun(arguments []string, global globalFlags) (int, error) {
	fs := newFlagSet("dry-run")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	repo := fs.String("repo", "", "repository")
	workflowValue := fs.String("workflow", "", "workflow name or TOML path")
	var verify stringListFlag
	fs.Var(&verify, "verify", "verification argv (repeatable)")
	noWorktree := fs.Bool("no-worktree", false, "explicitly do not create a dedicated worktree")
	taskOption := fs.String("task", "", "task")
	positionalArgs, err := parseInterleaved(fs, arguments)
	if err != nil {
		return 2, err
	}
	repoPath, workflow, verifyCommands, err := loadPlanInputs(*repo, *workflowValue, verify)
	if err != nil {
		return 1, err
	}
	_, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	if err := requireManaged(client); err != nil {
		return 1, err
	}
	payload := DryRunPayload(repoPath, workflow, taskValue(*taskOption, strings.Join(positionalArgs, " ")), verifyCommands, !*noWorktree)
	if err := printJSON(payload); err != nil {
		return 1, err
	}
	return 0, nil
}

//nolint:go-bare-error -- CLI handlers return errors to Main, which serializes them as JSON.
func runResume(arguments []string, global globalFlags) (int, error) {
	fs := newFlagSet("resume")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	runID := fs.String("run-id", "", "run id")
	foreground := fs.Bool("foreground", false, "accepted for compatibility")
	_ = foreground
	if err := fs.Parse(arguments); err != nil {
		return 2, err
	}
	if *runID == "" {
		err := fmt.Errorf("--run-id is required")
		return 2, err
	}
	store, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	if err := requireManaged(client); err != nil {
		return 1, err
	}
	state, err := store.Load(*runID)
	if err != nil {
		return 1, err
	}
	workflow, err := workflowFromState(state)
	if err != nil {
		return 1, err
	}
	result, err := NewFlowEngine(store, client).ResumeRun(*runID, &workflow)
	if err != nil {
		return 1, err
	}
	if err := printJSON(result); err != nil {
		return 1, err
	}
	code := stateExitCode(result)
	return code, nil
}

//nolint:go-bare-error -- CLI handlers return errors to Main, which serializes them as JSON.
func runStatus(arguments []string, global globalFlags, full bool) (int, error) {
	fs := newFlagSet("status")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	runIDOption := fs.String("run-id", "", "run id")
	step := fs.String("step", "", "step id")
	positionalArgs, err := parseInterleaved(fs, arguments)
	if err != nil {
		return 2, err
	}
	store, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	if err := requireManaged(client); err != nil {
		return 1, err
	}
	positional := ""
	if len(positionalArgs) > 0 {
		positional = positionalArgs[0]
	}
	runID, err := pickRunID(store, positional, *runIDOption)
	if err != nil {
		return 1, err
	}
	state, err := store.Load(runID)
	if err != nil {
		return 1, err
	}
	var output any
	if full && *step != "" {
		stepValue := mapValue(stateMap(state, "steps")[*step])
		if stepValue == nil {
			return 1, flowError("step not found: %s", *step)
		}
		output = stepValue
	} else if full {
		output = state
	} else {
		output = CompactState(state)
	}
	if err := printJSON(output); err != nil {
		return 1, err
	}
	code := stateExitCode(state)
	return code, nil
}

func runList(arguments []string, global globalFlags) (int, error) {
	fs := newFlagSet("list")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	if err := fs.Parse(arguments); err != nil {
		return 2, err
	}
	store, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	if err := requireManaged(client); err != nil {
		return 1, err
	}
	entries, err := store.List()
	if err != nil {
		return 1, err
	}
	if err := printJSON(entries); err != nil {
		return 1, err
	}
	return 0, nil
}

//nolint:go-bare-error -- CLI handlers return errors to Main, which serializes them as JSON.
func runWait(arguments []string, global globalFlags) (int, error) {
	fs := newFlagSet("wait")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	runIDOption := fs.String("run-id", "", "run id")
	timeoutValue := fs.String("timeout-seconds", strconv.FormatFloat(DefaultWaitTimeoutSeconds, 'f', -1, 64), "wait timeout")
	intervalValue := fs.String("interval-seconds", strconv.FormatFloat(DefaultWaitIntervalSeconds, 'f', -1, 64), "state polling interval")
	if err := fs.Parse(arguments); err != nil {
		return 2, err
	}
	timeout, err := positiveSeconds(*timeoutValue)
	if err != nil {
		return 2, err
	}
	interval, err := intervalSeconds(*intervalValue)
	if err != nil {
		return 2, err
	}
	store, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	if err := requireManaged(client); err != nil {
		return 1, err
	}
	runID, err := pickRunID(store, "", *runIDOption)
	if err != nil {
		return 1, err
	}
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, syscall.SIGINT)
	defer signal.Stop(interrupted)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-interrupted:
			cancel()
		case <-ctx.Done():
		}
	}()
	state, timedOut, err := WaitForTerminalContext(ctx, store, runID, time.Duration(timeout*float64(time.Second)), time.Duration(interval*float64(time.Second)))
	if errors.Is(err, context.Canceled) {
		latest, loadErr := store.Load(runID)
		if loadErr != nil {
			return 1, loadErr
		}
		if printErr := printJSON(CompactState(latest)); printErr != nil {
			return 1, printErr
		}
		return 130, nil
	}
	if err != nil {
		return 1, err
	}
	if err := printJSON(CompactState(state)); err != nil {
		return 1, err
	}
	if timedOut {
		return 124, nil
	}
	code := waitExitCode(state)
	return code, nil
}

//nolint:go-bare-error -- CLI handlers return errors to Main, which serializes them as JSON.
func runCancel(arguments []string, global globalFlags) (int, error) {
	fs := newFlagSet("cancel")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	positionalArgs, err := parseInterleaved(fs, arguments)
	if err != nil {
		return 2, err
	}
	if len(positionalArgs) == 0 {
		err := fmt.Errorf("run_id is required")
		return 2, err
	}
	store, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	if err := requireManaged(client); err != nil {
		return 1, err
	}
	state, err := NewFlowEngine(store, client).CancelRun(positionalArgs[0])
	if err != nil {
		return 1, err
	}
	if err := printJSON(CompactState(state)); err != nil {
		return 1, err
	}
	code := stateExitCode(state)
	return code, nil
}

func runCleanup(arguments []string, global globalFlags) (int, error) {
	fs := newFlagSet("cleanup")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	runID := fs.String("run-id", "", "run id")
	allRuns := fs.Bool("all", false, "all runs")
	olderThan := fs.Float64("older-than-hours", 24, "hours since last update")
	removeWorktree := fs.Bool("remove-worktree", false, "also remove the dedicated worktree")
	if err := fs.Parse(arguments); err != nil {
		return 2, err
	}
	store, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	if err := requireManaged(client); err != nil {
		return 1, err
	}
	result, err := NewFlowEngine(store, client).Cleanup(*runID, *allRuns, *olderThan, *removeWorktree)
	if err != nil {
		return 1, err
	}
	if err := printJSON(result); err != nil {
		return 1, err
	}
	return 0, nil
}

func runCleanupResources(arguments []string, global globalFlags) (int, error) {
	fs := newFlagSet("cleanup-resources")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	runID := fs.String("run-id", "", "run id")
	if err := fs.Parse(arguments); err != nil {
		return 2, err
	}
	if *runID == "" {
		return 2, fmt.Errorf("--run-id is required")
	}
	store, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	if err := requireManaged(client); err != nil {
		return 1, err
	}
	state, err := NewFlowEngine(store, client).CleanupResources(*runID, true)
	if err != nil {
		return 1, err
	}
	if err := printJSON(CompactState(state)); err != nil {
		return 1, err
	}
	return 0, nil
}

func runDoctor(arguments []string, global globalFlags) (int, error) {
	fs := newFlagSet("doctor")
	stateRoot, herdrBin := addCommonFlags(fs, global)
	repo := fs.String("repo", "", "repository")
	workflowValue := fs.String("workflow", "", "workflow name or TOML path")
	var verify stringListFlag
	fs.Var(&verify, "verify", "verification argv (repeatable)")
	if err := fs.Parse(arguments); err != nil {
		return 2, err
	}
	store, client, err := newRuntime(globalFlags{stateRoot: *stateRoot, herdrBin: *herdrBin})
	if err != nil {
		return 1, err
	}
	checks := []any{}
	envOK := client.Environ["HERDR_ENV"] == "1"
	checks = append(checks, map[string]any{"name": "HERDR_ENV", "ok": envOK, "detail": map[bool]string{true: "1", false: "HERDR_ENV=1 is required"}[envOK]})
	executablePathValue := client.ResolvedExecutable()
	executableOK := false
	if info, err := os.Stat(client.Executable); err == nil && !info.IsDir() {
		executableOK = true
	} else if _, err := exec.LookPath(client.Executable); err == nil {
		executableOK = true
	}
	checks = append(checks, map[string]any{"name": "herdr_executable", "ok": executableOK, "detail": map[bool]string{true: executablePathValue, false: client.Executable}[executableOK]})
	stateOK := true
	stateDetail := store.Root
	probe := filepath.Join(store.Root, ".doctor-write-test")
	if err := os.WriteFile(probe, []byte("ok\n"), 0o600); err != nil {
		stateOK = false
		stateDetail = err.Error()
	} else if err := os.Remove(probe); err != nil && !os.IsNotExist(err) {
		stateOK = false
		stateDetail = err.Error()
	}
	checks = append(checks, map[string]any{"name": "state_directory", "ok": stateOK, "detail": stateDetail})
	version := ""
	if envOK && executableOK {
		if value, err := client.RunText([]string{"--version"}, 10*time.Second); err == nil {
			version = value
		}
	}
	versionOK := strings.HasPrefix(version, "herdr 0.8.2")
	checks = append(checks, map[string]any{"name": "herdr_version", "ok": versionOK, "detail": valueOr(version, "unavailable (0.8.2 recommended)")})
	if *repo != "" || *workflowValue != "" {
		repoPath, workflow, verifyCommands, planErr := loadPlanInputs(*repo, *workflowValue, verify)
		if planErr != nil {
			checks = append(checks, map[string]any{"name": "workflow", "ok": false, "detail": planErr.Error()})
		} else {
			checks = append(checks, map[string]any{"name": "workflow", "ok": true, "detail": workflow.Path})
			verifyJSON := []any{}
			for _, command := range verifyCommands {
				verifyJSON = append(verifyJSON, command)
			}
			checks = append(checks, map[string]any{"name": "verification_commands", "ok": len(verifyCommands) > 0, "detail": verifyJSON})
			checks = append(checks, map[string]any{"name": "repo", "ok": directoryExists(repoPath), "detail": repoPath})
		}
	}
	allOK := true
	for _, raw := range checks {
		if !boolValue(mapValue(raw)["ok"], false) {
			allOK = false
		}
	}
	if err := printJSON(map[string]any{"ok": allOK, "checks": checks}); err != nil {
		return 1, err
	}
	return map[bool]int{true: 0, false: 1}[allOK], nil
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func Main(arguments []string) int {
	global, command, rest, err := parseGlobalPrefix(arguments)
	if err != nil {
		if printErr := printJSON(map[string]any{"error": err.Error(), "type": errorTypeName(err)}); printErr != nil {
			fmt.Fprintln(os.Stderr, printErr)
		}
		// Python argparse exits with 2 on usage errors (missing/unknown command).
		return 2
	}
	var code int
	switch command {
	case "start":
		code, err = runStart(rest, global)
	case "dry-run":
		code, err = runDryRun(rest, global)
	case "resume":
		code, err = runResume(rest, global)
	case "status":
		code, err = runStatus(rest, global, false)
	case "show":
		code, err = runStatus(rest, global, true)
	case "wait":
		code, err = runWait(rest, global)
	case "list":
		code, err = runList(rest, global)
	case "cancel":
		code, err = runCancel(rest, global)
	case "cleanup":
		code, err = runCleanup(rest, global)
	case "cleanup-resources":
		code, err = runCleanupResources(rest, global)
	case "doctor":
		code, err = runDoctor(rest, global)
	default:
		code, err = 2, fmt.Errorf("unknown command: %s", command)
	}
	if err != nil {
		if printErr := printJSON(map[string]any{"error": err.Error(), "type": errorTypeName(err)}); printErr != nil {
			fmt.Fprintln(os.Stderr, printErr)
		}
		if code == 0 {
			return 1
		}
	}
	return code
}
