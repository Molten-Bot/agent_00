package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/customlibrary"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/githubreview"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"github.com/Molten-Bot/agent_00/internal/library"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/multiplex"
	"github.com/Molten-Bot/agent_00/internal/web"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

const failureFollowUpRequiredPrompt = failurefollowup.RequiredPrompt

const gitHubCLIPackageLabel = "github-cli (gh)"
const gitHubCLIAuthRecommendation = "Run `gh auth login` (the GitHub CLI binary from the `github-cli` package) or set GH_TOKEN before running tasks."
const followUpTaskLogArchiveSubdir = "followup"
const localSubmitSource = "local_submit"
const libraryScheduleSource = "library_schedule"
const reviewWatchSource = "github_review_watch"
const prReviewFeedbackSource = "pr_review_feedback"
const rerunSource = "rerun"
const failureFollowUpSource = "failure_followup"
const noChangesFollowUpSource = "no_changes_followup"
const noChangesEscalationSource = "no_changes_escalation"
const automaticFailureRerunDisabledReason = "automatic failure rerun disabled; queue failure follow-up in agent_00"
const bootDiagnosticTimeout = 10 * time.Second

var runSingleHarness = func(ctx context.Context, cfg config.Config, logf func(string, ...any)) app.Result {
	h := app.New(execx.OSRunner{})
	h.Logf = logf
	h.LoadSigning = signingSettings(localruntime.InitConfig{}, h.Runner).Snapshot
	return h.Run(ctx, cfg)
}

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		printUsage()
		return app.ExitUsage
	}

	switch os.Args[1] {
	case "run":
		if err := workspace.PrepareDefaultRoots(); err != nil {
			fmt.Fprintf(os.Stderr, "workspace init error: %v\n", err)
			return app.ExitWorkspace
		}
		return runSingle(os.Args[2:])
	case "multiplex":
		if err := workspace.PrepareDefaultRoots(); err != nil {
			fmt.Fprintf(os.Stderr, "workspace init error: %v\n", err)
			return app.ExitWorkspace
		}
		return runMultiplex(os.Args[2:])
	case "serve":
		if err := workspace.PrepareDefaultRoots(); err != nil {
			fmt.Fprintf(os.Stderr, "workspace init error: %v\n", err)
			return app.ExitWorkspace
		}
		return runServer(os.Args[2:])
	default:
		printUsage()
		return app.ExitUsage
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: harness run --config <path-to-json>")
	fmt.Fprintln(os.Stderr, "   or: harness multiplex --config <path-or-dir> [--config <path-or-dir> ...] [--parallel <n>]")
	fmt.Fprintln(os.Stderr, "   or: harness serve [--init <path-to-init-json> | --config <path-to-config-json>] [--parallel <n>] [--ui-listen <host:port>] [--ui-automatic]")
}

func runSingle(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "Path to run configuration JSON")
	if err := fs.Parse(args); err != nil {
		return app.ExitUsage
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "missing required --config flag")
		return app.ExitUsage
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return app.ExitConfig
	}

	logger := newDefaultTerminalLogger()
	defer logger.Close()

	result := runSingleHarness(context.Background(), cfg, logger.Printf)
	if failureResult, failed := localNoChangesFailureResult(localSubmitSource, result, cfg); failed {
		result = failureResult
	}
	if result.Err != nil {
		writeStderrLine(logger, failureMessage(result.Err.Error()))
		if result.WorkspaceDir != "" {
			writeStderrLine(logger, fmt.Sprintf("workspace: %s", result.WorkspaceDir))
		}
		return result.ExitCode
	}

	if result.NoChanges && !resultHasPR(result) {
		var line strings.Builder
		line.WriteString(fmt.Sprintf("status=no_changes workspace=%s branch=%s", result.WorkspaceDir, result.Branch))
		if result.PRURL != "" {
			line.WriteString(fmt.Sprintf(" pr_url=%s", result.PRURL))
		}
		if prURLs := joinAllPRURLs(result.RepoResults); prURLs != "" {
			line.WriteString(fmt.Sprintf(" pr_urls=%s", prURLs))
		}
		writeStdoutLine(logger, line.String())
		return app.ExitSuccess
	}
	var line strings.Builder
	line.WriteString(fmt.Sprintf("status=completed workspace=%s branch=%s", result.WorkspaceDir, result.Branch))
	if result.PRURL != "" {
		line.WriteString(fmt.Sprintf(" pr_url=%s", result.PRURL))
	}
	if prURLs := completedPRURLs(result); prURLs != "" {
		line.WriteString(fmt.Sprintf(" pr_urls=%s", prURLs))
	}
	if changedRepos := countChangedRepos(result.RepoResults); changedRepos > 0 {
		line.WriteString(fmt.Sprintf(" changed_repos=%d", changedRepos))
	}
	writeStdoutLine(logger, line.String())

	return app.ExitSuccess
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func runMultiplex(args []string) int {
	fs := flag.NewFlagSet("multiplex", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var configInputs stringListFlag
	fs.Var(&configInputs, "config", "Path to task config JSON file or directory (repeatable)")
	parallel := fs.Int("parallel", 2, "Maximum number of parallel sessions")

	if err := fs.Parse(args); err != nil {
		return app.ExitUsage
	}
	configInputs = append(configInputs, fs.Args()...)
	if len(configInputs) == 0 {
		fmt.Fprintln(os.Stderr, "missing required --config flag")
		return app.ExitUsage
	}

	configPaths, err := collectConfigPaths(configInputs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config discovery error: %v\n", err)
		return app.ExitConfig
	}
	if len(configPaths) == 0 {
		fmt.Fprintln(os.Stderr, "no task config files found")
		return app.ExitConfig
	}

	logger := newDefaultTerminalLogger()
	defer logger.Close()
	mx := multiplex.New(execx.OSRunner{})
	mx.MaxParallel = *parallel
	mx.Logf = logger.Printf
	mx.LoadSigning = signingSettings(localruntime.InitConfig{}, execx.OSRunner{}).Snapshot

	result := mx.Run(context.Background(), configPaths)
	for _, s := range result.Sessions {
		var line strings.Builder
		line.WriteString(fmt.Sprintf("session=%s status=%s config=%s stage=%s", s.ID, s.State, s.ConfigPath, s.Stage))
		if s.ExitCode != app.ExitSuccess {
			line.WriteString(fmt.Sprintf(" exit_code=%d", s.ExitCode))
		}
		if s.WorkspaceDir != "" {
			line.WriteString(fmt.Sprintf(" workspace=%s", s.WorkspaceDir))
		}
		if s.Branch != "" {
			line.WriteString(fmt.Sprintf(" branch=%s", s.Branch))
		}
		if s.PRURL != "" {
			line.WriteString(fmt.Sprintf(" pr_url=%s", s.PRURL))
		}
		if s.Error != "" {
			line.WriteString(fmt.Sprintf(" err=%q", s.Error))
		}
		writeStdoutLine(logger, line.String())
	}

	return result.ExitCode()
}

func runServer(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	initPath := fs.String("init", "", "Path to local init JSON")
	configPath := fs.String("config", "", "Path to local runtime config JSON")
	parallel := fs.Int("parallel", 0, "Optional override for scheduler max parallel workers")
	uiListen := fs.String("ui-listen", "127.0.0.1:7777", "Local web UI listen address")
	uiAutomatic := fs.Bool("ui-automatic", false, "Hide the browser Studio form and run the monitor UI in automatic mode")

	if err := fs.Parse(args); err != nil {
		return app.ExitUsage
	}
	if strings.TrimSpace(*uiListen) == "" {
		fmt.Fprintln(os.Stderr, "serve requires --ui-listen; use run or multiplex for noninteractive tasks")
		return app.ExitUsage
	}
	cfg, exitCode, err := loadServerBootConfig(*initPath, *configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCode
	}
	if *parallel > 0 {
		cfg.Scheduler.MaxParallel = *parallel
	}

	logger := newDefaultTerminalLogger()
	defer logger.Close()
	runner := execx.OSRunner{}
	monitorBroker := web.NewBroker()
	logLevel, err := localruntime.ParseLogLevel(cfg.LogLevel)
	if err != nil {
		logLevel = localruntime.LogLevelInfo
	}
	logRouter := newRuntimeLogRouter(logger, monitorBroker, logLevel)
	daemonLogger := logRouter.Printf

	runtimeCfg, runtimeErr := agentruntime.Resolve(cfg.AgentHarness, cfg.AgentCommand)
	var authGate agentAuthGate

	localSubmitDeduper := newLocalSubmissionDeduper(localSubmissionDedupTTL)
	localTaskController := newLocalTaskController()
	var localTaskSeq uint64

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if runtimeErr == nil {
		authGate = newAgentAuthGate(ctx, runner, runtimeCfg, cfg, daemonLogger)
	}

	scheduler := localruntime.NewAdaptiveScheduler(cfg.Scheduler, daemonLogger)
	scheduler.Start(ctx)

	logRoot := ""
	if mirror, ok := logger.sink.(*taskLogMirror); ok {
		logRoot = strings.TrimSpace(mirror.rootDir)
	}

	var queueFailureRerun func(failedRequestID string, failedResult app.Result, failedRunCfg config.Config, source string)
	var queueFailureFollowUp func(failedRequestID string, failedResult app.Result, failedRunCfg config.Config, source string)
	var queueUnexpectedNoChangesFollowUp func(requestID string, result app.Result, runCfg config.Config)
	var queueEscalatedNoChangesFollowUp func(requestID string, result app.Result, runCfg config.Config)
	var enqueueLocalRun func(reqCtx context.Context, runCfg config.Config, allowFailureFollowUp bool, source string, force bool) (string, error)
	enqueueLocalRun = func(reqCtx context.Context, runCfg config.Config, allowFailureFollowUp bool, source string, force bool) (string, error) {
		if authGate != nil {
			authState, authErr := authGate.Status(reqCtx)
			if authErr != nil {
				return "", fmt.Errorf("check agent auth status: %w", authErr)
			}
			if authState.Required && !authState.Ready {
				return "", fmt.Errorf(
					"agent auth required: %s",
					firstNonEmptyString(authState.Message, "complete agent authorization in the UI"),
				)
			}
		}

		activeCfg, err := effectiveRuntimeConfig(cfg)
		if err != nil {
			return "", fmt.Errorf("load runtime config: %w", err)
		}
		runCfg = applyDefaultAgentRuntimeConfig(runCfg, activeCfg)
		runCfg, err = localruntime.ApplyBoundAgentRuntime(runCfg, activeCfg)
		if err != nil {
			return "", err
		}
		if err := validateRunConfigPromptImages(runCfg); err != nil {
			return "", err
		}
		source = strings.TrimSpace(source)
		if source == "" {
			source = localSubmitSource
		}

		dedupeKey := dedupeKeyForSubmission(runCfg, source)
		if force {
			dedupeKey = ""
		}
		allowCompletedDuplicate := source == rerunSource || source == libraryScheduleSource
		if dedupeKey != "" {
			if duplicate, state, duplicateOf := localSubmitDeduper.Check(dedupeKey, allowCompletedDuplicate); duplicate {
				daemonLogger(
					"task status=duplicate source=%s state=%s duplicate_of=%s",
					source,
					state,
					duplicateOf,
				)
				return "", newDuplicateSubmissionError(duplicateOf, state)
			}
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("service is shutting down")
		default:
		}
		select {
		case <-reqCtx.Done():
			return "", fmt.Errorf("request canceled")
		default:
		}

		requestID := fmt.Sprintf(
			"local-%d-%06d",
			time.Now().UTC().Unix(),
			atomic.AddUint64(&localTaskSeq, 1),
		)
		if dedupeKey != "" {
			if accepted, state, duplicateOf := localSubmitDeduper.Begin(dedupeKey, requestID, allowCompletedDuplicate); !accepted {
				daemonLogger(
					"task status=duplicate source=%s state=%s duplicate_of=%s",
					source,
					state,
					duplicateOf,
				)
				return "", newDuplicateSubmissionError(duplicateOf, state)
			}
		}
		runCtx, cancelRun := context.WithCancelCause(ctx)
		taskHandle := localTaskController.Register(requestID, cancelRun)
		if runConfigJSON, ok := marshalRunConfigJSON(runCfg); ok {
			monitorBroker.RecordTaskRunConfigWithSource(requestID, runConfigJSON, taskStartSourceForSubmission(source, runCfg))
		}

		go func(
			requestID string,
			runCfg config.Config,
			activeCfg localruntime.InitConfig,
			dedupeKey string,
			source string,
			allowFailureFollowUp bool,
			runCtx context.Context,
			cancelRun context.CancelCauseFunc,
			taskHandle *localTaskHandle,
		) {
			finalState := ""
			if dedupeKey != "" {
				defer func() {
					localSubmitDeduper.Done(dedupeKey, requestID, dedupeFinalStateForSubmission(source, finalState))
				}()
			}
			defer cancelRun(nil)
			defer localTaskController.Complete(requestID)

			for {
				if taskHandle != nil {
					if waitErr := taskHandle.WaitUntilRunnable(runCtx); waitErr != nil {
						if errors.Is(waitErr, errTaskStoppedByOperator) {
							finalState = "stopped"
							daemonLogger("task status=stopped request_id=%s err=%q", requestID, waitErr)
							return
						}
						finalState = "error"
						daemonLogger("task status=error request_id=%s err=%q", requestID, waitErr)
						if !errors.Is(waitErr, context.Canceled) {
							failedResult := app.Result{
								ExitCode: app.ExitPreflight,
								Err:      fmt.Errorf("task wait: %w", waitErr),
							}
							if allowFailureFollowUp {
								if queueFailureRerun != nil {
									queueFailureRerun(requestID, failedResult, runCfg, source)
								}
								if queueFailureFollowUp != nil {
									queueFailureFollowUp(requestID, failedResult, runCfg, source)
								}
							}
						}
						return
					}
				}

				acquireCtx, cancelAcquire := context.WithCancel(runCtx)
				if taskHandle != nil {
					taskHandle.SetAcquireCancel(cancelAcquire)
				}
				forceAcquire := false
				if taskHandle != nil {
					forceAcquire = taskHandle.ConsumeForceAcquire()
				}
				acquire := scheduler.Acquire
				if forceAcquire {
					acquire = scheduler.AcquireForce
				}
				release, acquireErr := acquire(acquireCtx, requestID)
				if taskHandle != nil {
					taskHandle.ClearAcquireCancel(cancelAcquire)
				}
				cancelAcquire()

				if acquireErr != nil {
					if taskHandle != nil && taskHandle.IsStopped() {
						finalState = "stopped"
						daemonLogger("task status=stopped request_id=%s err=%q", requestID, errTaskStoppedByOperator)
						return
					}
					if taskHandle != nil && taskHandle.IsPaused() && errors.Is(acquireErr, context.Canceled) && runCtx.Err() == nil {
						continue
					}
					if taskHandle != nil && taskHandle.HasForceAcquire() && errors.Is(acquireErr, context.Canceled) && runCtx.Err() == nil {
						continue
					}
					finalState = "error"
					daemonLogger("task status=error request_id=%s err=%q", requestID, acquireErr)
					if !errors.Is(acquireErr, context.Canceled) {
						failedResult := app.Result{
							ExitCode: app.ExitPreflight,
							Err:      fmt.Errorf("task acquire: %w", acquireErr),
						}
						if allowFailureFollowUp {
							if queueFailureRerun != nil {
								queueFailureRerun(requestID, failedResult, runCfg, source)
							}
							if queueFailureFollowUp != nil {
								queueFailureFollowUp(requestID, failedResult, runCfg, source)
							}
						}
					}
					return
				}

				if taskHandle != nil {
					taskHandle.SetRunning(true)
				}
				finalReviewPasses := activeCfg.ReviewWatch.ReviewCount()
				if startCfg, startCfgErr := effectiveRuntimeConfig(activeCfg); startCfgErr == nil {
					finalReviewPasses = startCfg.ReviewWatch.ReviewCount()
				}
				outcome := runLocalTask(runCtx, runner, daemonLogger, "local", requestID, runCfg, finalReviewPasses, func() bool {
					if taskHandle == nil {
						return false
					}
					return taskHandle.IsStopped()
				}, activeCfg)
				if taskHandle != nil {
					taskHandle.SetRunning(false)
				}
				release()

				finalState = outcome.State
				switch outcome.State {
				case "error":
					if errors.Is(outcome.Result.Err, agentruntime.ErrClaudeAuthRequired) {
						if gate, ok := authGate.(interface{ RequireReauthentication() }); ok {
							gate.RequireReauthentication()
						}
					}
					if allowFailureFollowUp {
						if queueFailureRerun != nil {
							queueFailureRerun(requestID, outcome.Result, runCfg, source)
						}
						if queueFailureFollowUp != nil {
							queueFailureFollowUp(requestID, outcome.Result, runCfg, source)
						}
					}
				case "no_changes":
					if failureResult, failed := localNoChangesFailureResult(source, outcome.Result, runCfg); failed {
						finalState = "error"
						daemonLogger(
							"task status=error request_id=%s exit_code=%d workspace=%s branch=%s pr_url=%s err=%q",
							requestID,
							failureResult.ExitCode,
							failureResult.WorkspaceDir,
							failureResult.Branch,
							failureResult.PRURL,
							failureResult.Err,
						)
					} else {
					}
					if source != noChangesFollowUpSource && source != noChangesEscalationSource && queueUnexpectedNoChangesFollowUp != nil {
						queueUnexpectedNoChangesFollowUp(requestID, outcome.Result, runCfg)
					}
					if queueEscalatedNoChangesFollowUp != nil {
						if ok, _ := shouldEscalateNoChangesFollowUp(source, outcome.Result, runCfg); ok {
							queueEscalatedNoChangesFollowUp(requestID, outcome.Result, runCfg)
						}
					}
				case "completed":
				}
				return
			}
		}(requestID, runCfg, activeCfg, dedupeKey, source, allowFailureFollowUp, runCtx, cancelRun, taskHandle)

		return requestID, nil
	}
	queueFailureRerun = func(failedRequestID string, failedResult app.Result, failedRunCfg config.Config, source string) {
		if ok, reason := shouldQueueFailureRerun(source, failedResult); !ok {
			daemonLogger(
				"task status=warn action=skip_failure_rerun request_id=%s err=%q",
				failedRequestID,
				reason,
			)
			return
		}

		rerunRequestID, rerunErr := enqueueFailureRerun(ctx, enqueueLocalRun, failedRunCfg)
		if rerunErr != nil {
			daemonLogger(
				"task status=warn action=queue_failure_rerun request_id=%s err=%q",
				failedRequestID,
				rerunErr,
			)
			return
		}
		daemonLogger(
			"task status=ok action=queue_failure_rerun request_id=%s rerun_request_id=%s",
			failedRequestID,
			rerunRequestID,
		)
	}
	queueFailureFollowUp = func(failedRequestID string, failedResult app.Result, failedRunCfg config.Config, source string) {
		if ok, reason := shouldQueueFailureFollowUp(source, failedResult); !ok {
			daemonLogger(
				"task status=warn action=skip_failure_followup request_id=%s err=%q",
				failedRequestID,
				reason,
			)
			return
		}

		followUpCfg := failureFollowUpRunConfig(failedRequestID, failedResult, failedRunCfg, logRoot)
		if len(followUpCfg.RepoList()) == 0 {
			daemonLogger(
				"task status=warn action=queue_failure_followup request_id=%s err=%q",
				failedRequestID,
				"no failed-task repo found for follow-up",
			)
			return
		}
		followUpRequestID, followUpErr := enqueueLocalRun(ctx, followUpCfg, true, failureFollowUpSource, false)
		if followUpErr != nil {
			daemonLogger(
				"task status=warn action=queue_failure_followup request_id=%s err=%q",
				failedRequestID,
				followUpErr,
			)
			return
		}
		daemonLogger(
			"task status=ok action=queue_failure_followup request_id=%s follow_up_request_id=%s",
			failedRequestID,
			followUpRequestID,
		)
	}
	queueUnexpectedNoChangesFollowUp = func(requestID string, result app.Result, runCfg config.Config) {
		logPaths := existingPaths(followUpTaskLogPaths(logRoot, requestID))
		if ok, reason := shouldQueueUnexpectedNoChangesFollowUpWithLogs(result, runCfg, logPaths); !ok {
			daemonLogger(
				"task status=warn action=skip_no_changes_followup request_id=%s err=%q",
				requestID,
				reason,
			)
			return
		}

		followUpCfg := unexpectedNoChangesFollowUpRunConfig(requestID, result, runCfg, logRoot)
		if len(followUpCfg.RepoList()) == 0 {
			daemonLogger(
				"task status=warn action=queue_no_changes_followup request_id=%s err=%q",
				requestID,
				"no task repo found for no-changes follow-up",
			)
			return
		}

		followUpRequestID, followUpErr := enqueueLocalRun(ctx, followUpCfg, true, noChangesFollowUpSource, false)
		if followUpErr != nil {
			daemonLogger(
				"task status=warn action=queue_no_changes_followup request_id=%s err=%q",
				requestID,
				followUpErr,
			)
			return
		}
		daemonLogger(
			"task status=ok action=queue_no_changes_followup request_id=%s follow_up_request_id=%s",
			requestID,
			followUpRequestID,
		)
	}
	queueEscalatedNoChangesFollowUp = func(requestID string, result app.Result, runCfg config.Config) {
		logPaths := existingPaths(followUpTaskLogPaths(logRoot, requestID))
		if ok, reason := shouldEscalateNoChangesFollowUpWithLogs(noChangesFollowUpSource, result, runCfg, logPaths); !ok {
			daemonLogger(
				"task status=warn action=skip_no_changes_escalation request_id=%s err=%q",
				requestID,
				reason,
			)
			return
		}

		followUpCfg := escalatedNoChangesFollowUpRunConfig(requestID, result, runCfg, logRoot)
		if len(followUpCfg.RepoList()) == 0 {
			daemonLogger(
				"task status=warn action=queue_no_changes_escalation request_id=%s err=%q",
				requestID,
				"no task repo found for no-changes escalation",
			)
			return
		}

		followUpRequestID, followUpErr := enqueueLocalRun(ctx, followUpCfg, false, noChangesEscalationSource, false)
		if followUpErr != nil {
			daemonLogger(
				"task status=warn action=queue_no_changes_escalation request_id=%s err=%q",
				requestID,
				followUpErr,
			)
			return
		}
		daemonLogger(
			"task status=ok action=queue_no_changes_escalation request_id=%s follow_up_request_id=%s",
			requestID,
			followUpRequestID,
		)
	}

	var reviewWatcher *githubreview.Watcher
	if strings.TrimSpace(*uiListen) != "" {
		uiServer := web.NewServer(*uiListen, monitorBroker)
		uiReady := make(chan error, 1)
		uiServer.Ready = uiReady
		uiServer.AutomaticMode = *uiAutomatic
		uiServer.ConfiguredHarness = cfg.AgentHarness
		uiServer.Logf = daemonLogger
		uiServer.LoadLibraryTasks = func() ([]library.TaskSummary, error) {
			var catalog library.Catalog
			var err error
			if localruntime.ReadRuntimeConfigDefaultLibraryEnabled(cfg.RuntimeConfigPath) {
				catalog, err = library.LoadCustomizedCatalog(library.DefaultDir)
			} else {
				catalog, err = library.LoadCustomLibraryCatalog()
			}
			if err != nil {
				return nil, err
			}
			usage := localruntime.ReadRuntimeConfigLibraryTaskUsage(cfg.RuntimeConfigPath)
			return library.OrderSummariesByUsage(catalog.Summaries(), usage), nil
		}
		cleanupTaskLogs := func(_ context.Context, requestID string) error {
			var errs []error

			logDir, ok := taskLogDir(logRoot, requestID)
			workspaceDir := ""
			if ok {
				workspaceDir = taskWorkspaceDirFromLogDir(logDir, requestID)
				if err := os.RemoveAll(logDir); err != nil {
					errs = append(errs, fmt.Errorf("remove task log dir %s: %w", logDir, err))
				} else {
					daemonLogger("task status=ok action=task_close_cleanup request_id=%s log_dir=%s", requestID, logDir)
				}
			}

			if workspaceDir != "" {
				switch removed, removeErr := removeManagedWorkspaceDir(workspaceDir); {
				case removeErr != nil:
					errs = append(errs, removeErr)
				case removed:
					daemonLogger("task status=ok action=task_close_cleanup request_id=%s workspace_dir=%s", requestID, workspaceDir)
				default:
					daemonLogger("task status=warn action=skip_workspace_cleanup request_id=%s workspace_dir=%s", requestID, workspaceDir)
				}
			}

			return errors.Join(errs...)
		}
		if authGate != nil {
			uiServer.AgentAuthStatus = authGate.Status
			uiServer.StartAgentAuth = authGate.StartDeviceAuth
			uiServer.VerifyAgentAuth = authGate.Verify
			if shouldEnableAgentAuthConfigure(runtimeCfg.Harness) {
				uiServer.ConfigureAgentAuth = authGate.Configure
			}
		}
		signing := signingSettings(cfg, runner)
		uiServer.CommitSigning = &signing
		uiServer.ReviewSettingsStatus = func(context.Context) (web.ReviewSettingsState, error) {
			return currentReviewSettingsState(cfg), nil
		}
		uiServer.ConfigureReviewSettings = func(_ context.Context, req web.ReviewSettingsRequest) (web.ReviewSettingsState, error) {
			state, err := configureReviewSettings(cfg, req)
			if err == nil && reviewWatcher != nil {
				activeCfg, loadErr := effectiveRuntimeConfig(cfg)
				if loadErr != nil {
					err = loadErr
				} else {
					reviewWatcher.UpdateOptions(reviewWatcherOptions(activeCfg.ReviewWatch))
				}
			}
			return state, err
		}
		recordLibraryUsage := func(runCfg config.Config) {
			if strings.TrimSpace(runCfg.LibraryTaskName) == "" {
				return
			}
			if err := localruntime.IncrementRuntimeConfigLibraryTaskUsage(cfg.RuntimeConfigPath, cfg, runCfg.LibraryTaskName); err != nil {
				daemonLogger("library.usage status=warn task=%s err=%q", runCfg.LibraryTaskName, err)
			}
		}

		schedules := &localruntime.LibrarySchedules{
			Path: cfg.RuntimeConfigPath, Init: cfg, Logf: daemonLogger,
			Submit: func(reqCtx context.Context, runCfg config.Config) (string, error) {
				id, err := enqueueLocalRun(reqCtx, runCfg, true, libraryScheduleSource, false)
				if err == nil {
					recordLibraryUsage(runCfg)
				}
				return id, err
			},
			Active: func(schedule localruntime.LibrarySchedule) bool {
				return libraryScheduleActive(monitorBroker, schedule)
			},
		}
		uiServer.LibrarySchedules = schedules
		go schedules.Run(ctx)

		customLibrarySources := &localruntime.CustomLibrarySources{
			Path:   cfg.RuntimeConfigPath,
			Init:   cfg,
			Logf:   daemonLogger,
			Syncer: customlibrary.Syncer{Root: customlibrary.DefaultRoot, Runner: runner},
			OnChanged: func() {
				monitorBroker.NotifyCustomLibraryUpdated()
			},
		}
		uiServer.CustomLibrarySources = customLibrarySources
		go customLibrarySources.Run(ctx, time.Hour)
		uiServer.SubmitLocalPrompt = func(reqCtx context.Context, body []byte) (string, error) {
			runCfg, err := localruntime.ParseRunConfigJSON(body)
			if err != nil {
				return "", fmt.Errorf("invalid run config: %w", err)
			}
			recordLibraryUsage(runCfg)
			return enqueueLocalRun(reqCtx, runCfg, true, localSubmitSource, false)
		}
		uiServer.SubmitTaskRerun = func(reqCtx context.Context, _ string, body []byte, force bool) (string, error) {
			runCfg, err := localruntime.ParseRunConfigJSON(body)
			if err != nil {
				return "", fmt.Errorf("invalid run config: %w", err)
			}
			recordLibraryUsage(runCfg)
			return enqueueLocalRun(reqCtx, runCfg, true, rerunSource, force)
		}
		uiServer.CloseTask = cleanupTaskLogs
		uiServer.ResolveTaskControls = localTaskController.Controls
		uiServer.PauseTask = func(_ context.Context, requestID string) error {
			if err := localTaskController.Pause(requestID); err != nil {
				return err
			}
			daemonLogger("task status=paused request_id=%s", requestID)
			return nil
		}
		uiServer.RunTask = func(_ context.Context, requestID string) error {
			if err := localTaskController.Run(requestID); err != nil {
				return err
			}
			daemonLogger("task status=resumed request_id=%s", requestID)
			return nil
		}
		uiServer.ForceRunTask = func(_ context.Context, requestID string) error {
			if err := localTaskController.ForceRun(requestID); err != nil {
				return err
			}
			daemonLogger("task status=forced request_id=%s", requestID)
			return nil
		}
		uiServer.StopTask = func(_ context.Context, requestID string) error {
			canonical, err := localTaskController.StopWithCanonical(requestID)
			if err != nil {
				return err
			}
			daemonLogger("task status=stopped request_id=%s err=%q", firstNonEmptyString(canonical, requestID), errTaskStoppedByOperator)
			return nil
		}
		go func() {
			if err := uiServer.Run(ctx); err != nil {
				daemonLogger("localruntime.ui status=error err=%q", err)
			}
		}()
		select {
		case err := <-uiReady:
			if err != nil {
				writeStderrLine(logger, fmt.Sprintf("error: %v", err))
				return app.ExitPreflight
			}
			daemonLogger("localruntime.ui status=ready url=%s", monitorURL(*uiListen))
		case <-ctx.Done():
			return app.ExitSuccess
		}
		prMonitor := &web.PRMergeMonitor{
			Runner:      runner,
			Broker:      monitorBroker,
			Logf:        daemonLogger,
			CleanupTask: cleanupTaskLogs,
			DeleteMergedBranchesEnabled: func() bool {
				activeCfg, err := effectiveRuntimeConfig(cfg)
				if err != nil {
					activeCfg = cfg
				}
				activeCfg.ApplyDefaults()
				return activeCfg.ReviewWatch.DeleteMergedBranchesEnabled()
			},
			OnReviewFeedback: func(reqCtx context.Context, task web.Task, feedback web.PRReviewFeedback) (string, error) {
				runConfigJSON, ok := monitorBroker.TaskRunConfig(task.RequestID)
				if !ok {
					return "", fmt.Errorf("stored run config not found for request %s", task.RequestID)
				}
				originalCfg, err := localruntime.ParseRunConfigJSON(runConfigJSON)
				if err != nil {
					return "", fmt.Errorf("parse stored run config for request %s: %w", task.RequestID, err)
				}
				followUpCfg, err := prReviewFeedbackRunConfig(originalCfg, task, feedback)
				if err != nil {
					return "", err
				}
				return enqueueLocalRun(reqCtx, followUpCfg, true, prReviewFeedbackSource, false)
			},
		}
		go func() {
			if err := prMonitor.Run(ctx); err != nil {
				daemonLogger("localruntime.ui status=warn event=pr_monitor err=%q", err)
			}
		}()
	}

	maybeStartAgentAuth(ctx, runtimeCfg, authGate, daemonLogger)
	reviewWatcherCfg := cfg
	if activeCfg, err := effectiveRuntimeConfig(cfg); err == nil {
		reviewWatcherCfg = activeCfg
	} else {
		daemonLogger("review_watch status=warn action=load_runtime_config err=%q", err)
	}
	reviewWatcher = startGitHubReviewWatcher(ctx, runner, reviewWatcherCfg, enqueueLocalRun, daemonLogger)
	runBootDiagnostics(ctx, runner, daemonLogger, cfg)
	<-ctx.Done()
	return app.ExitSuccess
}

func loadServerBootConfig(initPath, configPath string) (localruntime.InitConfig, int, error) {
	initPath = strings.TrimSpace(initPath)
	configPath = strings.TrimSpace(configPath)

	if initPath != "" && configPath != "" {
		return localruntime.InitConfig{}, app.ExitUsage, fmt.Errorf("provide only one of --init or --config")
	}

	if configPath != "" {
		return finalizeServerBootConfig(loadServerBootRuntimeConfigOrDefault(configPath))
	}

	if initPath != "" {
		cfg, err := localruntime.LoadInit(initPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				runtimePath := localruntime.ResolveRuntimeConfigPath(initPath)
				runtimeCfg, runtimeErr := localruntime.LoadRuntimeConfig(runtimePath)
				if runtimeErr == nil {
					cfg := runtimeCfg.Init()
					cfg.RuntimeConfigPath = runtimeCfg.RuntimeConfigPath
					return finalizeServerBootConfig(cfg, app.ExitSuccess, nil)
				}
				return finalizeServerBootConfig(loadServerBootRuntimeConfigOrDefault(runtimePath))
			}
			return localruntime.InitConfig{}, app.ExitConfig, fmt.Errorf("init config error: %w", err)
		}
		cfg.RuntimeConfigPath = localruntime.ResolveRuntimeConfigPath(initPath)
		cfg.LogLevel = configuredLogLevel(cfg.LogLevel, cfg.RuntimeConfigPath)
		return finalizeServerBootConfig(cfg, app.ExitSuccess, nil)
	}

	return finalizeServerBootConfig(loadServerBootRuntimeConfigOrDefault(""))
}

func loadServerBootRuntimeConfigOrDefault(path string) (localruntime.InitConfig, int, error) {
	runtimeCfg, err := localruntime.LoadRuntimeConfig(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return localruntime.InitConfig{}, app.ExitConfig, err
		}
		resolvedPath := strings.TrimSpace(path)
		if resolvedPath == "" {
			resolvedPath = localruntime.ResolveRuntimeConfigPath("")
		}
		return defaultServerBootConfig(resolvedPath)
	}

	cfg := runtimeCfg.Init()
	cfg.RuntimeConfigPath = runtimeCfg.RuntimeConfigPath
	return cfg, app.ExitSuccess, nil
}

func finalizeServerBootConfig(cfg localruntime.InitConfig, exitCode int, err error) (localruntime.InitConfig, int, error) {
	if err != nil {
		return cfg, exitCode, err
	}
	if token := githubTokenFromEnv(); token != "" {
		cfg.GitHubToken = token
		if err := localruntime.SaveRuntimeConfigGitHubToken(cfg.RuntimeConfigPath, cfg, token); err != nil {
			return cfg, app.ExitConfig, err
		}
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return cfg, app.ExitConfig, err
	}
	return cfg, exitCode, nil
}

func githubTokenFromEnv() string {
	return configuredGitHubTokenFromEnv()
}

func configuredGitHubTokenFromEnv() string {
	if token := strings.TrimSpace(os.Getenv("GH_TOKEN")); token != "" {
		return token
	}
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token
	}
	return malformedDockerComposeGitHubTokenFromEnv()
}

func malformedDockerComposeGitHubTokenFromEnv() string {
	return malformedDockerComposeEnvValue("GH_TOKEN", "GITHUB_TOKEN")
}

func malformedDockerComposeEnvValue(names ...string) string {
	for _, entry := range os.Environ() {
		for _, name := range names {
			prefix := name + ":"
			if !strings.HasPrefix(entry, prefix) {
				continue
			}
			token := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(entry, prefix), "="))
			if token != "" {
				return token
			}
		}
	}
	return ""
}

func configuredLogLevel(preferred, runtimeConfigPath string) string {
	level := localruntime.NormalizeLogLevel(preferred)
	if level == "" {
		level = localruntime.DefaultLogLevel
	}

	runtimeConfigPath = strings.TrimSpace(runtimeConfigPath)
	if runtimeConfigPath == "" {
		return level
	}

	configured := localruntime.NormalizeLogLevel(localruntime.ReadRuntimeConfigString(runtimeConfigPath, "log_level", "logLevel", "LOG_LEVEL"))
	if configured != "" {
		return configured
	}

	if runtimeCfg, err := localruntime.LoadRuntimeConfig(runtimeConfigPath); err == nil {
		if configured = localruntime.NormalizeLogLevel(runtimeCfg.LogLevel); configured != "" {
			return configured
		}
	}

	return level
}

func defaultServerBootConfig(runtimeConfigPath string) (localruntime.InitConfig, int, error) {
	cfg := localruntime.InitConfig{
		RuntimeConfigPath: strings.TrimSpace(runtimeConfigPath),
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return localruntime.InitConfig{}, app.ExitConfig, fmt.Errorf("init config error: %w", err)
	}
	return cfg, app.ExitSuccess, nil
}

type runtimeLogRouter struct {
	logger *terminalLogger
	broker *web.Broker
	level  localruntime.LogLevel
}

func newRuntimeLogRouter(logger *terminalLogger, broker *web.Broker, level localruntime.LogLevel) runtimeLogRouter {
	return runtimeLogRouter{
		logger: logger,
		broker: broker,
		level:  level,
	}
}

func (r runtimeLogRouter) Printf(format string, args ...any) {
	r.Log(strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (r runtimeLogRouter) Log(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	if r.broker != nil {
		r.broker.IngestLog(line)
	}

	eventLevel := classifyRuntimeLogLine(line)
	if !r.level.Allows(eventLevel) {
		if r.logger != nil {
			// Keep filtered lines in the task mirror for follow-up diagnostics.
			r.logger.Capture(line)
		}
		return
	}

	if r.logger != nil {
		r.logger.Print(line)
	}
}

func classifyRuntimeLogLine(line string) localruntime.LogLevel {
	line = strings.TrimSpace(line)
	if line == "" {
		return localruntime.LogLevelInfo
	}

	lowerLine := strings.ToLower(line)
	if strings.HasPrefix(lowerLine, "error:") {
		return localruntime.LogLevelError
	}
	if strings.HasPrefix(lowerLine, "warn:") {
		return localruntime.LogLevelWarn
	}
	if strings.HasPrefix(line, "debug ") {
		return localruntime.LogLevelDebug
	}

	fields := parseSimpleKVFields(line)
	status := strings.ToLower(strings.TrimSpace(fields["status"]))
	switch status {
	case "error", "failed", "invalid":
		return localruntime.LogLevelError
	case "warn":
		return localruntime.LogLevelWarn
	case "running":
		return localruntime.LogLevelDebug
	}

	if shouldClassifyRuntimeDebugLine(line, lowerLine, fields, status) {
		return localruntime.LogLevelDebug
	}

	return localruntime.LogLevelInfo
}

func shouldClassifyRuntimeDebugLine(line, lowerLine string, fields map[string]string, status string) bool {
	if isDebugCommandLogLine(line, fields) {
		return true
	}
	if strings.TrimSpace(fields["stage"]) != "" {
		return true
	}
	if strings.HasPrefix(lowerLine, "task status=") && isDebugTaskStatus(status) {
		return true
	}
	return false
}

func isDebugTaskStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "start", "ack", "queued", "duplicate", "paused", "resumed", "forced", "running":
		return true
	default:
		return false
	}
}
func isDebugCommandLogLine(line string, fields map[string]string) bool {
	if len(fields) == 0 {
		return false
	}
	phase := strings.TrimSpace(fields["phase"])
	if phase == "" {
		return false
	}
	return strings.HasPrefix(line, "cmd ") || strings.Contains(line, " cmd ")
}

func writeStdoutLine(logger *terminalLogger, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	fmt.Fprintln(os.Stdout, line)
	if logger != nil {
		logger.Capture(line)
	}
}

func writeStderrLine(logger *terminalLogger, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	fmt.Fprintln(os.Stderr, line)
	if logger != nil {
		logger.Capture(line)
	}
}

type localTaskOutcome struct {
	State  string
	Result app.Result
}

func runLocalTask(
	ctx context.Context,
	runner execx.Runner,
	logf func(string, ...any),
	skill string,
	requestID string,
	runCfg config.Config,
	finalReviewPasses int,
	wasStopped func() bool,
	signingConfigs ...localruntime.InitConfig,
) localTaskOutcome {
	logf(
		"task status=start request_id=%s skill=%s repo=%s repos=%s",
		requestID,
		skill,
		runCfg.RepoURL,
		strings.Join(runCfg.RepoList(), ","),
	)

	h := app.New(runner)
	h.FinalReviewPasses = finalReviewPasses
	var signingCfg localruntime.InitConfig
	if len(signingConfigs) > 0 {
		signingCfg = signingConfigs[0]
	}
	h.LoadSigning = signingSettings(signingCfg, runner).Snapshot
	h.Logf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		logf("task request_id=%s %s", requestID, line)
	}

	res := h.Run(ctx, runCfg)
	if res.Err != nil {
		if wasStopped != nil && wasStopped() {
			stopErr := context.Cause(ctx)
			if stopErr == nil {
				stopErr = errTaskStoppedByOperator
			}
			logf(
				"task status=stopped request_id=%s exit_code=%d workspace=%s branch=%s pr_url=%s err=%q",
				requestID,
				res.ExitCode,
				res.WorkspaceDir,
				res.Branch,
				res.PRURL,
				stopErr,
			)
			return localTaskOutcome{State: "stopped", Result: res}
		}
		logf(
			"task status=error request_id=%s exit_code=%d workspace=%s branch=%s pr_url=%s err=%q",
			requestID,
			res.ExitCode,
			res.WorkspaceDir,
			res.Branch,
			res.PRURL,
			res.Err,
		)
		return localTaskOutcome{State: "error", Result: res}
	}
	if res.NoChanges && !resultHasPR(res) {
		logf(
			"task status=no_changes request_id=%s workspace=%s branch=%s pr_url=%s pr_urls=%s",
			requestID,
			res.WorkspaceDir,
			res.Branch,
			res.PRURL,
			joinAllPRURLs(res.RepoResults),
		)
		return localTaskOutcome{State: "no_changes", Result: res}
	}
	logf(
		"task status=completed request_id=%s workspace=%s branch=%s pr_url=%s pr_urls=%s changed_repos=%d",
		requestID,
		res.WorkspaceDir,
		res.Branch,
		res.PRURL,
		completedPRURLs(res),
		countChangedRepos(res.RepoResults),
	)
	return localTaskOutcome{State: "completed", Result: res}
}

func failureFollowUpRunConfig(
	failedRequestID string,
	failedResult app.Result,
	failedRunCfg config.Config,
	logRoot string,
) config.Config {
	logPaths := followUpTaskLogPaths(logRoot, failedRequestID)
	baseBranch, targetSubdir := failurefollowup.FollowUpTargeting(
		failedRunCfg.BaseBranch,
		failedRunCfg.TargetSubdir,
		failedResult.Branch,
	)
	return config.Config{
		Repos:        failureFollowUpRepos(failedResult, failedRunCfg),
		BaseBranch:   baseBranch,
		TargetSubdir: targetSubdir,
		Prompt:       failureFollowUpPrompt(logPaths, failedResult, failedRunCfg),
	}
}

func unexpectedNoChangesFollowUpRunConfig(
	requestID string,
	result app.Result,
	runCfg config.Config,
	logRoot string,
) config.Config {
	runCfg.ApplyDefaults()
	logPaths := existingPaths(followUpTaskLogPaths(logRoot, requestID))
	baseBranch, targetSubdir := failurefollowup.FollowUpTargeting(
		runCfg.BaseBranch,
		runCfg.TargetSubdir,
		result.Branch,
	)
	return config.Config{
		Repos:        unexpectedNoChangesFollowUpRepos(runCfg),
		BaseBranch:   baseBranch,
		TargetSubdir: targetSubdir,
		Prompt:       unexpectedNoChangesFollowUpPrompt(logPaths, requestID, result, runCfg),
	}
}

func shouldQueueFailureFollowUp(source string, failedResult app.Result) (bool, string) {
	if errors.Is(failedResult.Err, gitsigning.ErrSetup) {
		return false, "commit signing requires configuration in Settings"
	}
	source = strings.TrimSpace(source)
	if errors.Is(failedResult.Err, agentruntime.ErrClaudeAuthRequired) {
		return false, "agent authentication requires user sign-in"
	}
	if failedResult.Err == nil {
		return false, "failed task did not include an error"
	}
	if errors.Is(failedResult.Err, agentruntime.ErrPromptImagesUnsupported) {
		return false, "prompt images are unsupported for the configured harness"
	}
	if source == failureFollowUpSource || source == noChangesEscalationSource {
		return false, "run is already a failure follow-up"
	}
	if reason := failurefollowup.NonRemediableFailureReason(failedResult.Err); reason != "" {
		return false, "failure is not remediable by code changes: " + reason
	}
	return true, ""
}

func shouldQueueFailureRerun(source string, failedResult app.Result) (bool, string) {
	if errors.Is(failedResult.Err, gitsigning.ErrSetup) {
		return false, "commit signing requires configuration in Settings"
	}
	source = strings.TrimSpace(source)
	if failedResult.Err == nil {
		return false, "failed task did not include an error"
	}
	if errors.Is(failedResult.Err, agentruntime.ErrPromptImagesUnsupported) {
		return false, "prompt images are unsupported for the configured harness"
	}
	switch source {
	case failureFollowUpSource, noChangesEscalationSource:
		return false, "run is already a failure follow-up"
	case rerunSource:
		return false, "run is already a failure rerun"
	}
	return false, automaticFailureRerunDisabledReason
}

type localRunEnqueueFunc func(context.Context, config.Config, bool, string, bool) (string, error)

func enqueueFailureRerun(ctx context.Context, enqueue localRunEnqueueFunc, failedRunCfg config.Config) (string, error) {
	if enqueue == nil {
		return "", fmt.Errorf("local run enqueuer is required")
	}
	// The failed run still owns the dedupe key until its goroutine unwinds, so
	// the required one-time rerun must bypass duplicate suppression.
	return enqueue(ctx, failedRunCfg, false, rerunSource, true)
}

func shouldQueueUnexpectedNoChangesFollowUp(result app.Result, runCfg config.Config) (bool, string) {
	if !result.NoChanges {
		return false, "task did not complete with no changes"
	}
	if strings.TrimSpace(joinAllPRURLs(result.RepoResults)) != "" || strings.TrimSpace(result.PRURL) != "" {
		return false, "task already has a pull request"
	}
	if result.NoChangeEvidence {
		return false, "agent cited concrete evidence that no repository changes are required"
	}
	if !promptRequestsRepositoryChange(runCfg.Prompt) {
		return false, "original prompt does not clearly require repository changes"
	}
	return true, ""
}

func shouldQueueUnexpectedNoChangesFollowUpWithLogs(result app.Result, runCfg config.Config, logPaths []string) (bool, string) {
	return shouldQueueUnexpectedNoChangesFollowUp(result, runCfg)
}

func shouldEscalateNoChangesFollowUp(source string, result app.Result, runCfg config.Config) (bool, string) {
	if strings.TrimSpace(source) != noChangesFollowUpSource {
		return false, "run is not a no-changes follow-up"
	}
	if !result.NoChanges {
		return false, "follow-up did not complete as no_changes"
	}
	if strings.TrimSpace(joinAllPRURLs(result.RepoResults)) != "" || strings.TrimSpace(result.PRURL) != "" {
		return false, "task already has a pull request"
	}
	if result.NoChangeEvidence {
		return false, "agent cited concrete evidence that no repository changes are required"
	}
	if !promptRequestsRepositoryChange(runCfg.Prompt) {
		return false, "original prompt does not clearly request repository changes"
	}
	return true, ""
}

func shouldEscalateNoChangesFollowUpWithLogs(source string, result app.Result, runCfg config.Config, logPaths []string) (bool, string) {
	return shouldEscalateNoChangesFollowUp(source, result, runCfg)
}

func localNoChangesFailureResult(source string, result app.Result, runCfg config.Config) (app.Result, bool) {
	if !result.NoChanges {
		return app.Result{}, false
	}
	if strings.TrimSpace(joinAllPRURLs(result.RepoResults)) != "" || strings.TrimSpace(result.PRURL) != "" {
		return app.Result{}, false
	}
	if result.NoChangeEvidence {
		return app.Result{}, false
	}
	if !promptRequestsRepositoryChange(runCfg.Prompt) {
		return app.Result{}, false
	}

	detail := noChangesFailureDetail(source)
	failed := result
	if failed.ExitCode == app.ExitSuccess {
		failed.ExitCode = app.ExitCodex
	}
	failed.Err = errors.New(detail)
	return failed, true
}

func noChangesFailureDetail(source string) string {
	rootCauses := "Root causes: no tracked file changes; no pull request URL; prompt requested repository changes; no concrete repository evidence justified a no-op."
	detail := "task completed with no file changes and no pull request even though the prompt appears to require repository changes"
	switch strings.TrimSpace(source) {
	case noChangesFollowUpSource:
		detail = "no-changes follow-up completed with no file changes and no pull request, without concrete evidence that no agent_00 change is required"
	case noChangesEscalationSource:
		detail = "no-changes escalation completed with no file changes and no pull request, without concrete evidence that no agent_00 change is required"
	}
	return detail + ". " + rootCauses
}

func failureFollowUpRepos(_ app.Result, _ config.Config) []string {
	if repo := strings.TrimSpace(failurefollowup.FollowUpRepositoryURL); repo != "" {
		return []string{repo}
	}
	return nil
}

func unexpectedNoChangesFollowUpRepos(runCfg config.Config) []string {
	return failureFollowUpRepos(app.Result{}, runCfg)
}

func unexpectedNoChangesFollowUpPrompt(logPaths []string, requestID string, result app.Result, runCfg config.Config) string {
	const requiredPrompt = "Review the previous local task logs first. The prior run completed with no file changes and no pull request, which is unexpected for this task. Identify every root cause behind the no-change result, fix the underlying agent_00 application issue in this repository, validate locally where possible, and summarize the verified results. Treat the original task prompt as failure context only; do not implement that requested product change here unless it is required to fix agent_00 failure handling. Only return a no-op if you can cite concrete repository evidence that no agent_00 change is required; otherwise produce the smallest correct diff or return an explicit failure with blocker details."

	var contextLines []string
	if requestID = strings.TrimSpace(requestID); requestID != "" {
		contextLines = append(contextLines, fmt.Sprintf("- request_id=%s", requestID))
	}
	if workspaceDir := strings.TrimSpace(result.WorkspaceDir); workspaceDir != "" {
		contextLines = append(contextLines, fmt.Sprintf("- workspace_dir=%s", workspaceDir))
	}
	if branch := strings.TrimSpace(result.Branch); branch != "" {
		contextLines = append(contextLines, fmt.Sprintf("- branch=%s", branch))
	}
	if repo := strings.Join(runCfg.RepoList(), ","); strings.TrimSpace(repo) != "" {
		contextLines = append(contextLines, fmt.Sprintf("- repos=%s", repo))
	}
	if targetSubdir := strings.TrimSpace(runCfg.TargetSubdir); targetSubdir != "" {
		contextLines = append(contextLines, fmt.Sprintf("- target_subdir=%s", targetSubdir))
	}

	var contextBlock strings.Builder
	if len(contextLines) > 0 {
		contextBlock.WriteString("Observed no-change context:\n")
		contextBlock.WriteString(strings.Join(contextLines, "\n"))
	}
	if prompt := strings.TrimSpace(runCfg.Prompt); prompt != "" {
		if contextBlock.Len() > 0 {
			contextBlock.WriteString("\n\n")
		}
		contextBlock.WriteString("Original task prompt:\n")
		contextBlock.WriteString(prompt)
	}

	return failurefollowup.ComposePrompt(
		requiredPrompt,
		logPaths,
		nil,
		"No local task log path was captured before the task completed without changes. Review the task history and runtime logs first.",
		contextBlock.String(),
	)
}

func escalatedNoChangesFollowUpRunConfig(
	requestID string,
	result app.Result,
	runCfg config.Config,
	logRoot string,
) config.Config {
	runCfg.ApplyDefaults()
	logPaths := existingPaths(followUpTaskLogPaths(logRoot, requestID))
	baseBranch, targetSubdir := failurefollowup.FollowUpTargeting(
		runCfg.BaseBranch,
		runCfg.TargetSubdir,
		result.Branch,
	)
	return config.Config{
		Repos:        unexpectedNoChangesFollowUpRepos(runCfg),
		BaseBranch:   baseBranch,
		TargetSubdir: targetSubdir,
		Prompt:       escalatedNoChangesFollowUpPrompt(logPaths, requestID, result, runCfg),
	}
}

func escalatedNoChangesFollowUpPrompt(logPaths []string, requestID string, result app.Result, runCfg config.Config) string {
	const requiredPrompt = "Review the previous local task logs first. The original task and the no-changes follow-up both completed without file changes or a pull request. For this escalation, fix the underlying agent_00 application issue in this repository. Treat the original task prompt as failure context only; do not implement that requested product change here unless it is required to fix agent_00 failure handling. Do not return another no-op unless you can cite exact file paths and concrete repository evidence proving no agent_00 change is required. If a real diff is blocked, return an explicit failure with the precise blocker details instead of another ambiguous no-op."

	var contextLines []string
	if requestID = strings.TrimSpace(requestID); requestID != "" {
		contextLines = append(contextLines, fmt.Sprintf("- request_id=%s", requestID))
	}
	if workspaceDir := strings.TrimSpace(result.WorkspaceDir); workspaceDir != "" {
		contextLines = append(contextLines, fmt.Sprintf("- workspace_dir=%s", workspaceDir))
	}
	if branch := strings.TrimSpace(result.Branch); branch != "" {
		contextLines = append(contextLines, fmt.Sprintf("- branch=%s", branch))
	}
	if repo := strings.Join(runCfg.RepoList(), ","); strings.TrimSpace(repo) != "" {
		contextLines = append(contextLines, fmt.Sprintf("- repos=%s", repo))
	}
	if targetSubdir := strings.TrimSpace(runCfg.TargetSubdir); targetSubdir != "" {
		contextLines = append(contextLines, fmt.Sprintf("- target_subdir=%s", targetSubdir))
	}

	var contextBlock strings.Builder
	if len(contextLines) > 0 {
		contextBlock.WriteString("Observed repeated no-change context:\n")
		contextBlock.WriteString(strings.Join(contextLines, "\n"))
	}
	if prompt := strings.TrimSpace(runCfg.Prompt); prompt != "" {
		if contextBlock.Len() > 0 {
			contextBlock.WriteString("\n\n")
		}
		contextBlock.WriteString("Original task prompt:\n")
		contextBlock.WriteString(prompt)
	}

	return failurefollowup.ComposePrompt(
		requiredPrompt,
		logPaths,
		nil,
		"No local task log path was captured before repeated no-change completions. Review the task history and runtime logs first.",
		contextBlock.String(),
	)
}

func promptRequestsRepositoryChange(prompt string) bool {
	text := strings.ToLower(strings.TrimSpace(prompt))
	if text == "" {
		return true
	}
	if isPlaceholderPrompt(text) {
		return false
	}

	if strings.Contains(text, "?") {
		questionPrefixes := []string{"what ", "why ", "how ", "when ", "where ", "who "}
		for _, prefix := range questionPrefixes {
			if strings.HasPrefix(text, prefix) {
				return false
			}
		}
	}

	if promptAllowsSuccessfulNoOp(text) {
		return false
	}

	changeMarkers := []string{
		"fix", "change", "update", "modify", "refactor", "add", "remove", "create",
		"implement", "make", "rename", "replace", "convert", "style", "color",
		"pink", "build", "ship", "wire", "hook up", "repair",
	}
	for _, marker := range changeMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}

	readOnlyMarkers := []string{
		"review", "investigate", "analysis", "analyze", "summarize", "summary",
		"explain", "inspect", "read", "show", "list", "find", "compare", "audit",
		"why", "what", "how",
	}
	for _, marker := range readOnlyMarkers {
		if strings.Contains(text, marker) {
			return false
		}
	}

	if strings.Contains(text, "?") {
		return false
	}
	return true
}

// isPlaceholderPrompt reports whether text is a bare connectivity/smoke-test
// prompt (e.g. "test", "ping") with no actionable content, as opposed to a
// genuine request that simply avoided the known marker words. Such prompts
// cannot be satisfied with a real diff, so they must not be treated as
// requiring a repository change.
func isPlaceholderPrompt(text string) bool {
	if len(strings.Fields(text)) > 2 {
		return false
	}
	switch strings.Trim(text, ".!? ") {
	case "test", "testing", "ping", "hello", "hi", "hey", "noop", "no-op", "sample", "example", "placeholder":
		return true
	}
	return false
}

func promptAllowsSuccessfulNoOp(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return false
	}
	phraseText := promptPhraseText(text)
	if promptExplicitlyAllowsNoChanges(phraseText) {
		return true
	}

	strongChangeMarkers := []string{
		"fix", "implement", "add", "remove", "create", "replace", "rename",
		"refactor", "repair", "build", "ship", "wire", "hook up",
		"change the", "change this", "modify", "update", "make the",
	}
	for _, marker := range strongChangeMarkers {
		if promptPhraseTextContains(phraseText, marker) {
			return false
		}
	}

	verificationMarkers := []string{
		"make sure", "ensure", "verify", "confirm", "check", "validate", "review",
		"inspect", "audit", "compare",
	}
	consistencyMarkers := []string{
		"up to date", "up-to-date", "current", "latest", "in sync", "out of sync",
		"drift", "no drift", "match", "matches", "matching", "consistent",
		"consistency",
	}

	hasVerification := false
	for _, marker := range verificationMarkers {
		if promptPhraseTextContains(phraseText, marker) {
			hasVerification = true
			break
		}
	}
	if !hasVerification {
		return false
	}
	for _, marker := range consistencyMarkers {
		if promptPhraseTextContains(phraseText, marker) {
			return true
		}
	}
	return false
}

func promptExplicitlyAllowsNoChanges(text string) bool {
	conditions := []string{
		"if there are no changes",
		"if there are no file changes",
		"if there are no repository changes",
		"if no changes",
		"when there are no changes",
		"when there are no file changes",
		"when there are no repository changes",
		"when no changes",
		"if nothing changed",
		"when nothing changed",
	}
	actions := []string{
		"do not produce",
		"do not create",
		"do not modify",
		"do not change",
		"don't produce",
		"don't create",
		"don't modify",
		"don't change",
		"return a no op",
		"produce no output",
		"make no changes",
		"leave unchanged",
		"skip the change",
		"skip creating",
	}

	const maxConditionalNoOpClauseChars = 240
	for _, condition := range conditions {
		condition = promptPhraseText(condition)
		remaining := text
		for {
			conditionIndex := strings.Index(remaining, condition)
			if conditionIndex < 0 {
				break
			}
			clause := remaining[conditionIndex:]
			if len(clause) > maxConditionalNoOpClauseChars {
				clause = clause[:maxConditionalNoOpClauseChars]
			}
			for _, action := range actions {
				if promptPhraseTextContains(clause, action) {
					return true
				}
			}
			remaining = remaining[conditionIndex+len(condition):]
		}
	}
	return false
}

func promptPhraseText(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(words) == 0 {
		return " "
	}
	return " " + strings.Join(words, " ") + " "
}

func promptPhraseTextContains(text, marker string) bool {
	return strings.Contains(text, promptPhraseText(marker))
}

func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func failureFollowUpPrompt(logPaths []string, failedResult app.Result, failedRunCfg config.Config) string {
	return failurefollowup.ComposePrompt(
		failureFollowUpRequiredPrompt,
		logPaths,
		[]string{
			".log/local/<request timestamp>/<request sequence>",
			".log/local/<request timestamp>/<request sequence>/term",
			".log/local/<request timestamp>/<request sequence>/terminal.log",
		},
		"",
		failureFollowUpFailureContext(failedResult, failedRunCfg),
	)
}

func failureFollowUpFailureContext(failedResult app.Result, failedRunCfg config.Config) string {
	lines := []string{
		"Observed failure context:",
		fmt.Sprintf("- exit_code=%d", failedResult.ExitCode),
	}
	if failedResult.Err != nil {
		lines = append(lines, fmt.Sprintf("- error=%q", failedResult.Err.Error()))
	}
	if workspaceDir := strings.TrimSpace(failedResult.WorkspaceDir); workspaceDir != "" {
		lines = append(lines, fmt.Sprintf("- workspace_dir=%s", workspaceDir))
	}
	if branch := strings.TrimSpace(failedResult.Branch); branch != "" {
		lines = append(lines, fmt.Sprintf("- branch=%s", branch))
	}
	if prURL := strings.TrimSpace(failedResult.PRURL); prURL != "" {
		lines = append(lines, fmt.Sprintf("- pr_url=%s", prURL))
	}
	if repos := failureFollowUpContextRepos(failedResult, failedRunCfg); len(repos) > 0 {
		lines = append(lines, fmt.Sprintf("- repos=%s", strings.Join(repos, ",")))
	}
	if targetSubdir := strings.TrimSpace(failedRunCfg.TargetSubdir); targetSubdir != "" {
		lines = append(lines, fmt.Sprintf("- target_subdir=%s", targetSubdir))
	}
	if prompt := strings.TrimSpace(failedRunCfg.Prompt); prompt != "" {
		lines = append(lines, "Original task prompt:")
		lines = append(lines, prompt)
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func failureFollowUpContextRepos(failedResult app.Result, failedRunCfg config.Config) []string {
	var repos []string
	seen := make(map[string]struct{})
	appendRepo := func(repo string) {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			return
		}
		if _, ok := seen[repo]; ok {
			return
		}
		seen[repo] = struct{}{}
		repos = append(repos, repo)
	}
	for _, repo := range failedRunCfg.RepoList() {
		appendRepo(repo)
	}
	for _, repoResult := range failedResult.RepoResults {
		appendRepo(repoResult.RepoURL)
	}
	return repos
}

func existingPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	existing := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		seen[path] = struct{}{}
		existing = append(existing, path)
	}
	if len(existing) == 0 {
		return nil
	}
	return existing
}

func followUpTaskLogPaths(logRoot, requestID string) []string {
	paths := taskLogPaths(logRoot, requestID)
	if _, ok := localTaskLogDir(logRoot, requestID); !ok {
		return paths
	}

	archived := archiveLocalTaskLogsForFollowUp(logRoot, requestID)
	if len(archived) > 0 {
		return archived
	}
	return paths
}

func archiveLocalTaskLogsForFollowUp(logRoot, requestID string) []string {
	sourceDir, ok := localTaskLogDir(logRoot, requestID)
	if !ok {
		return nil
	}
	sourceDir = strings.TrimSpace(sourceDir)
	if sourceDir == "" {
		return nil
	}
	if stat, err := os.Stat(sourceDir); err != nil || !stat.IsDir() {
		return nil
	}

	archiveDir, ok := followUpTaskLogArchiveDir(logRoot, requestID)
	if !ok {
		return nil
	}
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return nil
	}

	paths := []string{archiveDir}
	for _, fileName := range []string{legacyTaskLogFileName, logFileName} {
		sourcePath := filepath.Join(sourceDir, fileName)
		archivePath := filepath.Join(archiveDir, fileName)
		copied, err := copyFileIfPresent(sourcePath, archivePath)
		if err != nil || !copied {
			continue
		}
		paths = append(paths, archivePath)
	}
	if len(paths) == 1 {
		return nil
	}
	return paths
}

func copyFileIfPresent(sourcePath, targetPath string) (bool, error) {
	sourcePath = strings.TrimSpace(sourcePath)
	targetPath = strings.TrimSpace(targetPath)
	if sourcePath == "" || targetPath == "" {
		return false, nil
	}

	in, err := os.Open(sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer in.Close()

	out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, maxLogFileOpenMode)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return false, err
	}
	if err := out.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func followUpTaskLogArchiveDir(logRoot, requestID string) (string, bool) {
	logRoot = strings.TrimSpace(logRoot)
	if logRoot == "" {
		return "", false
	}
	subdir, ok := failurefollowup.IdentifierSubdir(requestID)
	if !ok {
		return "", false
	}
	subdir = filepath.Clean(subdir)
	if subdir == "." || subdir == "" || subdir == ".." {
		return "", false
	}
	if filepath.IsAbs(subdir) || strings.HasPrefix(subdir, ".."+string(filepath.Separator)) {
		return "", false
	}

	return filepath.Join(logRoot, followUpTaskLogArchiveSubdir, subdir), true
}

func taskLogPaths(logRoot, requestID string) []string {
	return failurefollowup.TaskLogPaths(logRoot, requestID)
}

func taskLogDir(logRoot, requestID string) (string, bool) {
	return failurefollowup.TaskLogDir(logRoot, requestID)
}

func localTaskLogDir(logRoot, requestID string) (string, bool) {
	subdir, ok := failurefollowup.IdentifierSubdir(requestID)
	if !ok {
		return "", false
	}
	subdir = filepath.Clean(subdir)
	localPrefix := "local" + string(filepath.Separator)
	if subdir != "local" && !strings.HasPrefix(subdir, localPrefix) {
		return "", false
	}

	return taskLogDir(logRoot, requestID)
}

func taskWorkspaceDirFromLogDir(logDir, requestID string) string {
	logDir = strings.TrimSpace(logDir)
	requestID = strings.TrimSpace(requestID)
	if logDir == "" || requestID == "" {
		return ""
	}
	for _, fileName := range []string{logFileName, legacyTaskLogFileName} {
		if workspaceDir := taskWorkspaceDirFromLogFile(filepath.Join(logDir, fileName), requestID); workspaceDir != "" {
			return workspaceDir
		}
	}
	return ""
}

func taskWorkspaceDirFromLogFile(logPath, requestID string) string {
	logPath = strings.TrimSpace(logPath)
	requestID = strings.TrimSpace(requestID)
	if logPath == "" || requestID == "" {
		return ""
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	return taskWorkspaceDirFromLogContent(string(content), requestID)
}

func taskWorkspaceDirFromLogContent(content, requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ""
	}

	lines := splitLogLines(content)
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "task status=") && !strings.HasPrefix(line, "dispatch status=") {
			continue
		}

		fields := parseSimpleKVFields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.TrimSpace(fields["request_id"]) != requestID {
			continue
		}
		status := strings.TrimSpace(fields["status"])
		switch status {
		case "completed", "no_changes", "error", "stopped", "ok":
		default:
			continue
		}

		workspaceDir := strings.TrimSpace(firstNonEmptyString(fields["workspace"], fields["workspace_dir"]))
		if workspaceDir == "" {
			continue
		}
		return workspaceDir
	}
	return ""
}

func splitLogLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return strings.Split(content, "\n")
}

func removeManagedWorkspaceDir(workspaceDir string) (bool, error) {
	workspaceDir = strings.TrimSpace(workspaceDir)
	if workspaceDir == "" {
		return false, nil
	}
	if !workspace.IsManagedRunDir(workspaceDir) {
		return false, nil
	}
	if err := os.RemoveAll(workspaceDir); err != nil {
		if !errors.Is(err, os.ErrPermission) {
			return false, fmt.Errorf("remove workspace dir %s: %w", workspaceDir, err)
		}
		// Go module caches contain read-only directories. Restore owner write
		// permission only inside this closed task before retrying cleanup.
		root, openErr := os.OpenRoot(workspaceDir)
		if openErr != nil {
			return false, fmt.Errorf("open workspace for cleanup: %w", openErr)
		}
		defer root.Close()
		walkErr := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return root.Chmod(path, info.Mode().Perm()|0700)
		})
		if walkErr != nil {
			return false, fmt.Errorf("prepare workspace cleanup: %w", walkErr)
		}
		if err := os.RemoveAll(workspaceDir); err != nil {
			return false, fmt.Errorf("remove workspace dir %s: %w", workspaceDir, err)
		}
	}
	return true, nil
}

func monitorURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	return "http://" + addr
}

func joinPRURLs(results []app.RepoResult) string {
	if len(results) == 0 {
		return ""
	}
	urls := make([]string, 0, len(results))
	for _, result := range results {
		if !result.Changed || strings.TrimSpace(result.PRURL) == "" {
			continue
		}
		urls = append(urls, strings.TrimSpace(result.PRURL))
	}
	return strings.Join(urls, ",")
}

func joinAllPRURLs(results []app.RepoResult) string {
	if len(results) == 0 {
		return ""
	}
	urls := make([]string, 0, len(results))
	for _, result := range results {
		if strings.TrimSpace(result.PRURL) == "" {
			continue
		}
		urls = append(urls, strings.TrimSpace(result.PRURL))
	}
	return strings.Join(urls, ",")
}

func resultHasPR(result app.Result) bool {
	if strings.TrimSpace(result.PRURL) != "" {
		return true
	}
	return strings.TrimSpace(joinAllPRURLs(result.RepoResults)) != ""
}

func completedPRURLs(result app.Result) string {
	if result.NoChanges {
		return joinAllPRURLs(result.RepoResults)
	}
	return joinPRURLs(result.RepoResults)
}

func countChangedRepos(results []app.RepoResult) int {
	count := 0
	for _, result := range results {
		if result.Changed {
			count++
		}
	}
	return count
}

func marshalRunConfigJSON(cfg config.Config) ([]byte, bool) {
	if strings.TrimSpace(cfg.LibraryTaskName) != "" {
		cfg.Prompt = ""
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, false
	}
	return payload, true
}

func taskStartSourceForSubmission(source string, runCfg config.Config) string {
	switch strings.TrimSpace(source) {
	case reviewWatchSource, prReviewFeedbackSource:
		return "review"
	case localSubmitSource, libraryScheduleSource:
		if strings.TrimSpace(runCfg.LibraryTaskName) != "" {
			return "library"
		}
		return "prompt"
	default:
		return ""
	}
}

func startGitHubReviewWatcher(
	ctx context.Context,
	runner execx.Runner,
	cfg localruntime.InitConfig,
	enqueueLocalRun func(context.Context, config.Config, bool, string, bool) (string, error),
	logf func(string, ...any),
) *githubreview.Watcher {
	if !cfg.ReviewWatch.EnabledValue() {
		return nil
	}
	if runner == nil || enqueueLocalRun == nil {
		if logf != nil {
			logf("review_watch status=warn action=start err=%q", "runner or enqueue function unavailable")
		}
		return nil
	}

	watcher := &githubreview.Watcher{
		Runner: runner,
		Enqueue: func(reqCtx context.Context, runCfg config.Config) (string, error) {
			return enqueueLocalRun(reqCtx, runCfg, true, reviewWatchSource, false)
		},
		Logf:    logf,
		Options: reviewWatcherOptions(cfg.ReviewWatch),
	}
	go func() {
		if logf != nil {
			logf("review_watch status=start interval_ms=%d", cfg.ReviewWatch.PollIntervalMS)
		}
		if err := watcher.Run(ctx); err != nil && logf != nil {
			logf("review_watch status=error err=%q", err)
		}
	}()
	return watcher
}

func reviewWatcherOptions(cfg localruntime.ReviewWatchConfig) githubreview.Options {
	cfg.MergeMethod = normalizeReviewSettingsMergeMethod(cfg.MergeMethod)
	if cfg.MergeMethod == "" {
		cfg.MergeMethod = "squash"
	}
	if cfg.PollIntervalMS <= 0 {
		cfg.PollIntervalMS = 60000
	}
	if strings.TrimSpace(cfg.Writeback) == "" {
		cfg.Writeback = "summary-comment"
	}
	if strings.TrimSpace(cfg.ResponseMode) == "" {
		cfg.ResponseMode = "off"
	}
	return githubreview.Options{
		PollInterval: time.Duration(cfg.PollIntervalMS) * time.Millisecond,
		Writeback:    cfg.Writeback,
		AutoMerge:    cfg.AutoMergeEnabled(),
		MergeMethod:  cfg.MergeMethod,
		ResponseMode: cfg.ResponseMode,
	}
}

func runBootDiagnostics(
	ctx context.Context,
	runner execx.Runner,
	logf func(string, ...any),
	cfg localruntime.InitConfig,
) {
	if runner == nil || logf == nil {
		return
	}
	runtime, err := agentruntime.Resolve(cfg.AgentHarness, cfg.AgentCommand)
	if err != nil {
		logf("boot.diagnosis status=error requirement=agent_runtime err=%q", err)
		return
	}

	checks := []struct {
		requirement string
		cmd         execx.Command
	}{
		{
			requirement: "git_cli",
			cmd:         execx.Command{Name: "git", Args: []string{"--version"}},
		},
		{
			requirement: "gh_cli",
			cmd:         execx.Command{Name: "gh", Args: []string{"--version"}},
		},
		{
			requirement: runtime.RequirementName(),
			cmd:         runtime.PreflightCommand(),
		},
	}
	checkNames := []string{"git_cli", "gh_cli", runtime.RequirementName(), "gh_auth"}
	logf("boot.diagnosis status=start checks=%s", strings.Join(checkNames, ","))

	failedRequiredChecks := 0
	for _, check := range checks {
		checkCtx, cancel := context.WithTimeout(ctx, bootDiagnosticTimeout)
		res, err := runner.Run(checkCtx, check.cmd)
		cancel()
		if err != nil {
			failedRequiredChecks++
			logf("boot.diagnosis status=error requirement=%s err=%q", check.requirement, err)
			continue
		}
		logf(
			"boot.diagnosis status=ok requirement=%s detail=%q",
			check.requirement,
			diagnosticDetailForResult(res),
		)
	}

	authCtx, cancel := context.WithTimeout(ctx, bootDiagnosticTimeout)
	authRes, authErr := runner.Run(authCtx, execx.Command{Name: "gh", Args: []string{"auth", "status"}})
	cancel()
	if authErr != nil {
		logf(
			"boot.diagnosis status=warn requirement=gh_auth detail=%q recommendation=%q",
			diagnosticDetailForResult(authRes),
			gitHubCLIAuthRecommendation,
		)
	} else {
		logf("boot.diagnosis status=ok requirement=gh_auth detail=%q", diagnosticDetailForResult(authRes))
	}

	if failedRequiredChecks == 0 {
		logf("boot.diagnosis status=complete required_checks=ok")
		return
	}
	logf(
		"boot.diagnosis status=warn required_checks=failed count=%d recommendation=%q",
		failedRequiredChecks,
		fmt.Sprintf(
			"Install missing tools before running tasks: git, %s, %s.",
			gitHubCLIPackageLabel,
			strings.TrimSpace(runtime.Command),
		),
	)
	return
}

func applyDefaultAgentRuntimeConfig(runCfg config.Config, initCfg localruntime.InitConfig) config.Config {
	if strings.TrimSpace(runCfg.AgentHarness) == "" {
		runCfg.AgentHarness = strings.TrimSpace(initCfg.AgentHarness)
	}
	if strings.TrimSpace(runCfg.AgentCommand) == "" {
		runCfg.AgentCommand = strings.TrimSpace(initCfg.AgentCommand)
	}
	return runCfg
}

func validateRunConfigPromptImages(runCfg config.Config) error {
	if len(runCfg.Images) == 0 {
		return nil
	}
	runtime, err := agentruntime.Resolve(runCfg.AgentHarness, runCfg.AgentCommand)
	if err != nil {
		return err
	}
	if agentruntime.SupportsPromptImages(runtime.Harness) {
		return nil
	}
	return agentruntime.UnsupportedPromptImagesError(runtime.Harness)
}

func maybeStartAgentAuth(ctx context.Context, runtime agentruntime.Runtime, gate agentAuthGate, logf func(string, ...any)) {
	if gate == nil || logf == nil {
		return
	}

	status, err := gate.Status(ctx)
	if err != nil {
		logf("localruntime.auth status=warn harness=%s action=check err=%q", runtime.Harness, err)
		return
	}
	if strings.TrimSpace(runtime.Harness) != agentruntime.HarnessClaude {
		return
	}
	if !status.Required || status.Ready {
		return
	}
	if strings.TrimSpace(status.State) == "pending_browser_login" {
		return
	}
	if strings.TrimSpace(status.State) == "needs_configure" &&
		strings.TrimSpace(status.ConfigureCommand) == claudeGitHubConfigureCommand {
		return
	}
	if strings.TrimSpace(status.AuthURL) != "" {
		return
	}

	started, startErr := gate.StartDeviceAuth(ctx)
	if startErr != nil {
		logf(
			"localruntime.auth status=warn harness=%s action=start_device_auth err=%q detail=%q",
			runtime.Harness,
			startErr,
			firstNonEmptyString(started.Message, status.Message),
		)
		return
	}
	logf(
		"localruntime.auth status=start harness=%s action=start_device_auth state=%s",
		runtime.Harness,
		firstNonEmptyString(started.State, "pending_browser_login"),
	)
}

func shouldEnableAgentAuthConfigure(harness string) bool {
	switch strings.TrimSpace(strings.ToLower(harness)) {
	case agentruntime.HarnessCodex, agentruntime.HarnessClaude:
		return true
	default:
		return false
	}
}

func diagnosticDetailForResult(res execx.Result) string {
	candidates := []string{res.Stdout, res.Stderr}
	for _, candidate := range candidates {
		for _, line := range strings.Split(candidate, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			line = strings.Join(strings.Fields(line), " ")
			const maxChars = 200
			if len(line) > maxChars {
				return line[:maxChars-3] + "..."
			}
			return line
		}
	}
	return "check completed"
}

func loadStoredRuntimeConfig(path string) (localruntime.InitConfig, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return localruntime.InitConfig{}, false, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return localruntime.InitConfig{}, false, nil
		}
		return localruntime.InitConfig{}, false, fmt.Errorf("read runtime config: %w", err)
	}

	var runtimeCfg localruntime.RuntimeConfig
	if err := json.Unmarshal(data, &runtimeCfg); err != nil {
		return localruntime.InitConfig{}, false, fmt.Errorf("parse runtime config: %w", err)
	}

	runtimeCfg.RuntimeConfigPath = path
	initCfg := runtimeCfg.Init()
	initCfg.RuntimeConfigPath = path
	return initCfg, true, nil
}

func effectiveRuntimeConfig(cfg localruntime.InitConfig) (localruntime.InitConfig, error) {
	activeCfg := cfg
	activeCfg.ApplyDefaults()

	storedCfg, found, err := loadStoredRuntimeConfig(cfg.RuntimeConfigPath)
	if err != nil {
		return activeCfg, err
	}
	if !found {
		return activeCfg, nil
	}

	if strings.TrimSpace(storedCfg.GitHubToken) == "" {
		storedCfg.GitHubToken = strings.TrimSpace(activeCfg.GitHubToken)
	}
	if strings.TrimSpace(storedCfg.AgentHarness) == "" {
		storedCfg.AgentHarness = strings.TrimSpace(activeCfg.AgentHarness)
	}
	if strings.TrimSpace(storedCfg.AgentCommand) == "" {
		storedCfg.AgentCommand = strings.TrimSpace(activeCfg.AgentCommand)
	}
	storedCfg.ApplyDefaults()
	return storedCfg, nil
}

func currentReviewSettingsState(cfg localruntime.InitConfig) web.ReviewSettingsState {
	activeCfg, err := effectiveRuntimeConfig(cfg)
	if err != nil {
		activeCfg = cfg
	}
	activeCfg.ApplyDefaults()
	return web.ReviewSettingsState{
		AutoMerge:             activeCfg.ReviewWatch.AutoMergeEnabled(),
		DeleteMergedBranches:  activeCfg.ReviewWatch.DeleteMergedBranchesEnabled(),
		MergeMethod:           activeCfg.ReviewWatch.MergeMethod,
		ReviewLevel:           activeCfg.ReviewWatch.ReviewLevel,
		DefaultLibraryEnabled: localruntime.ReadRuntimeConfigDefaultLibraryEnabled(cfg.RuntimeConfigPath),
	}
}

func configureReviewSettings(cfg localruntime.InitConfig, req web.ReviewSettingsRequest) (web.ReviewSettingsState, error) {
	method := normalizeReviewSettingsMergeMethod(req.MergeMethod)
	if method == "" {
		state := currentReviewSettingsState(cfg)
		return state, fmt.Errorf("review merge method must be merge, squash, or rebase")
	}
	rawReviewLevel := strings.TrimSpace(req.ReviewLevel)
	if rawReviewLevel == "" {
		rawReviewLevel = currentReviewSettingsState(cfg).ReviewLevel
	}
	reviewLevel := localruntime.NormalizeReviewLevel(rawReviewLevel)
	if reviewLevel == "" {
		state := currentReviewSettingsState(cfg)
		return state, fmt.Errorf("review level must be off, low, medium, or high")
	}

	autoMerge := req.AutoMerge
	deleteMergedBranches := req.DeleteMergedBranches
	reviewCfg := localruntime.ReviewWatchConfig{
		AutoMerge:            &autoMerge,
		DeleteMergedBranches: &deleteMergedBranches,
		MergeMethod:          method,
		ReviewLevel:          reviewLevel,
	}
	if err := localruntime.SaveRuntimeConfigReviewSettings(cfg.RuntimeConfigPath, cfg, reviewCfg); err != nil {
		state := currentReviewSettingsState(cfg)
		return state, err
	}
	if err := localruntime.SaveRuntimeConfigDefaultLibraryEnabled(cfg.RuntimeConfigPath, cfg, req.DefaultLibraryEnabled); err != nil {
		state := currentReviewSettingsState(cfg)
		return state, err
	}
	state := currentReviewSettingsState(cfg)
	state.Message = "Review settings saved."
	return state, nil
}

func normalizeReviewSettingsMergeMethod(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "squash":
		return "squash"
	case "merge":
		return "merge"
	case "rebase":
		return "rebase"
	default:
		return ""
	}
}

func collectConfigPaths(inputs []string) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	seen := map[string]struct{}{}
	var paths []string
	addPath := func(path string) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}

	for _, input := range inputs {
		in := strings.TrimSpace(input)
		if in == "" {
			continue
		}
		abs, err := filepath.Abs(in)
		if err != nil {
			return nil, fmt.Errorf("resolve path %q: %w", in, err)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", abs, err)
		}

		if !st.IsDir() {
			addPath(abs)
			continue
		}

		var discovered []string
		err = filepath.WalkDir(abs, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if strings.EqualFold(filepath.Ext(path), ".json") {
				discovered = append(discovered, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %q: %w", abs, err)
		}

		sort.Strings(discovered)
		for _, p := range discovered {
			addPath(p)
		}
	}

	return paths, nil
}

func failureMessage(text string) string {
	if strings.TrimSpace(text) == "" {
		text = "unknown error"
	}
	return "Failure: task failed.\nError details: " + strings.TrimSpace(text)
}
