package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

const RetryBaseSeconds int64 = 60
const RetryCapSeconds int64 = 1800

var safeRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
var safeErrorTypePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,79}$`)

type Graphify struct {
	client               GraphifyClient
	save                 func(State) error
	now                  func() int64
	logs                 io.Writer
	ctx                  context.Context
	worker               *CaptureWorker
	workerExecID         string
	captureStartReported bool
}

func NewGraphify(client GraphifyClient, save func(State) error, now func() int64, logs io.Writer) *Graphify {
	if save == nil {
		save = func(State) error { return nil }
	}
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	return &Graphify{client: client, save: save, now: now, logs: logs, ctx: context.Background()}
}
func (g *Graphify) Shutdown() {
	if g.worker == nil || !g.worker.IsAlive() {
		return
	}
	g.worker.Cancel()
	<-g.worker.Done()
}
func RetryDelay(failures int) int64 {
	if failures <= 1 {
		return RetryBaseSeconds
	}
	if failures >= 6 {
		return RetryCapSeconds
	}
	return int64(math.Min(float64(RetryCapSeconds), float64(RetryBaseSeconds)*math.Pow(2, float64(failures-1))))
}
func (g *Graphify) Tick(state State) {
	defer func() {
		if recover() != nil {
			g.log("error", "unresolved_exec", nil)
		}
	}()
	g.tick(state)
}

func (g *Graphify) tick(state State) {
	if g.ctx.Err() != nil {
		return
	}
	now := g.now()
	attempt := graphAttempt(state)
	if attempt != nil {
		g.advance(state, attempt, now)
		return
	}
	if g.worker != nil {
		if g.worker.IsAlive() {
			return
		}
		g.worker = nil
		g.workerExecID = ""
	}
	dirty := dirtyRepositories(state)
	if len(dirty) == 0 {
		return
	}
	eligible := false
	for _, name := range dirty {
		due, has := numberValue(stateEntryMap(state, name)["retry_due"])
		if !has || now >= due {
			eligible = true
			break
		}
	}
	if !eligible {
		return
	}
	snapshot := map[string]any{}
	for _, name := range dirty {
		snapshot[name] = stateEntryMap(state, name)["target"]
	}
	attempt = map[string]any{"phase": "pre_create", "start_requested": false, "exec_id": nil, "container_id": nil, "fallback_deadline": now + maxRetryFor(state, dirty), "snapshot": snapshot}
	setGraphAttempt(state, attempt)
	if !g.persist(state) {
		setGraphAttempt(state, nil)
		g.log("error", "persist_failure", map[string]any{"repo": safeRepo(dirty[0]), "not_persisted": true})
		return
	}
	g.create(state, attempt, now)
}

func (g *Graphify) create(state State, attempt map[string]any, now int64) {
	if g.ctx.Err() != nil {
		return
	}
	beforeCreate := cloneStateValue(state)
	containerID, err := g.client.ContainerID(g.ctx)
	if g.ctx.Err() != nil {
		return
	}
	if err == nil {
		var execID string
		execID, err = g.client.ExecCreate(g.ctx, containerID, []string{"graphify", "update", "/workspace"}, []string{"GRAPHIFY_OUT=/data"})
		if err == nil {
			attempt["exec_id"], attempt["container_id"], attempt["phase"] = execID, containerID, "pre_start"
		} else if g.ctx.Err() != nil {
			return
		}
	}
	if err != nil {
		g.log("error", "exec_create_error", map[string]any{"status": safeDockerStatus(err), "error_type": errorType(err)})
		before := beforeCreate
		setGraphAttempt(state, nil)
		g.recordFailures(state, attemptSnapshot(attempt), now)
		if !g.persist(state) {
			restoreState(state, before)
			g.log("error", "persist_failure", map[string]any{"not_persisted": true})
			return
		}
		g.log("info", "retry_scheduled", map[string]any{"repos": safeRepos(attemptSnapshot(attempt)), "retry_delay": retryDelayRemaining(state, attemptSnapshot(attempt), now)})
		return
	}
	if !g.persist(state) {
		restoreState(state, beforeCreate)
		g.log("error", "persist_failure", map[string]any{"not_persisted": true})
		return
	}
	g.start(state, attempt)
}

func (g *Graphify) start(state State, attempt map[string]any) {
	if g.ctx.Err() != nil {
		return
	}
	id, _ := attempt["exec_id"].(string)
	if !dockerID.MatchString(id) {
		g.log("error", "unresolved_exec", map[string]any{"reason": "invalid_saved_id"})
		return
	}
	if g.worker != nil && g.worker.IsAlive() {
		return
	}
	before := cloneStateValue(state)
	attempt["start_requested"], attempt["phase"] = true, "pre_start"
	if !g.persist(state) {
		restoreState(state, before)
		g.log("error", "persist_failure", map[string]any{"not_persisted": true})
		return
	}
	if g.ctx.Err() != nil {
		return
	}
	g.captureStartReported = false
	g.workerExecID = id
	g.worker = newCaptureWorker(g.ctx, func(ctx context.Context) (string, error) {
		err := g.client.ExecStart(ctx, id)
		if err != nil {
			var diagnostic *captureDiagnosticError
			if errors.As(err, &diagnostic) {
				return diagnostic.class, err
			}
			return "unavailable", err
		}
		return "unclassified", nil
	}, nil)
	g.worker.Start()
}

func (g *Graphify) advance(state State, attempt map[string]any, now int64) {
	if g.ctx.Err() != nil {
		return
	}
	id, _ := attempt["exec_id"].(string)
	if id == "" {
		gate, ok := numberValue(attempt["fallback_deadline"])
		if !ok || now >= gate {
			g.create(state, attempt, now)
		}
		return
	}
	if !dockerID.MatchString(id) {
		g.log("error", "unresolved_exec", map[string]any{"reason": "invalid_saved_id"})
		return
	}
	inspection, err := g.client.ExecInspect(g.ctx, id)
	if g.ctx.Err() != nil {
		return
	}
	if err != nil {
		status := safeDockerStatus(err)
		if status == "unknown" {
			g.log("error", "exec_inspect_error", map[string]any{"error_type": errorType(err)})
		} else {
			g.log("error", "unresolved_exec", map[string]any{"status": status})
		}
		return
	}
	if inspection.Running {
		g.log("info", "exec_running", nil)
		return
	}
	if inspection.ExitCode != nil {
		if g.worker != nil && g.workerExecID == id && g.worker.IsAlive() {
			g.worker.Cancel()
			if g.worker.IsAlive() {
				return
			}
		}
		if *inspection.ExitCode == 0 {
			g.terminalSuccess(state, attempt)
			return
		}
		diagnostic := "unavailable"
		if g.worker != nil && g.workerExecID == id {
			diagnostic = g.worker.Result()
		}
		g.terminalFailure(state, attempt, now, *inspection.ExitCode, diagnostic)
		return
	}
	if g.worker != nil && g.workerExecID == id && !g.worker.IsAlive() {
		err := g.worker.Error()
		var diagnostic *captureDiagnosticError
		if err != nil && !errors.As(err, &diagnostic) && !g.captureStartReported {
			var apiErr *DockerAPIError
			if errors.As(err, &apiErr) {
				g.log("error", "exec_start_unconfirmed", map[string]any{"status": safeDockerStatus(err)})
			} else {
				g.log("error", "exec_start_unconfirmed", map[string]any{"error_type": "unknown"})
			}
			g.captureStartReported = true
		}
		var apiErr *DockerAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != 409 {
			g.observedStartFailure(state, attempt, now)
			return
		}
	}
	if now >= startGate(state, attempt) && (g.worker == nil || !g.worker.IsAlive()) {
		g.start(state, attempt)
	}
}

func (g *Graphify) terminalSuccess(state State, attempt map[string]any) {
	snapshot := attemptSnapshot(attempt)
	before := cloneStateValue(state)
	for repo, generation := range attemptGenerationSnapshot(attempt) {
		entry := stateEntryMap(state, repo)
		if sameGeneration(entry["target"], generation) {
			entry["completed"] = generation
			delete(entry, "failures")
			delete(entry, "retry_due")
		} else {
			entry["completed"] = generation
		}
	}
	if !g.persist(state) {
		restoreState(state, before)
		g.log("error", "persist_failure", map[string]any{"not_persisted": true})
		return
	}
	acknowledged := cloneStateValue(state)
	setGraphAttempt(state, nil)
	if !g.persist(state) {
		restoreState(state, acknowledged)
		g.log("error", "persist_failure", map[string]any{"not_persisted": true})
		return
	}
	g.log("info", "exec_succeeded", map[string]any{"repos": safeRepos(snapshot)})
}

func (g *Graphify) terminalFailure(state State, attempt map[string]any, now int64, exitCode int, diagnostic string) {
	affected := dirtyRepositories(state)
	if len(affected) == 0 {
		affected = attemptSnapshot(attempt)
	}
	candidate := cloneStateValue(state)
	setGraphAttempt(candidate, nil)
	g.recordFailures(candidate, affected, now)
	if !g.persist(candidate) {
		g.log("error", "persist_failure", map[string]any{"not_persisted": true})
		return
	}
	restoreState(state, candidate)
	g.log("error", "exec_failed", map[string]any{"exit_code": exitCode, "repos": safeRepos(affected), "diagnostic_class": safeDiagnostic(diagnostic)})
}

func (g *Graphify) observedStartFailure(state State, attempt map[string]any, now int64) {
	repos := attemptSnapshot(attempt)
	candidate := cloneStateValue(state)
	setGraphAttempt(candidate, nil)
	g.recordFailures(candidate, repos, now)
	if !g.persist(candidate) {
		g.log("error", "persist_failure", map[string]any{"not_persisted": true})
		return
	}
	restoreState(state, candidate)
	g.log("info", "retry_scheduled", map[string]any{"repos": safeRepos(repos), "retry_delay": retryDelayRemaining(state, repos, now)})
}

func (g *Graphify) recordFailures(state State, repos []string, now int64) {
	for _, repo := range repos {
		entry := stateEntryMap(state, repo)
		failures, _ := numberValue(entry["failures"])
		failures++
		entry["failures"] = failures
		entry["retry_due"] = now + RetryDelay(int(failures))
	}
}
func (g *Graphify) persist(state State) bool { return g.save(state) == nil }
func (g *Graphify) log(level, outcome string, fields map[string]any) {
	if g.logs == nil {
		return
	}
	if !validGraphifyOutcome(outcome) {
		outcome = "unresolved_exec"
	}
	safeFields := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		safeFields[key] = value
	}
	safeFields["outcome"] = outcome
	WriteLog(g.logs, level, "graphify_refresh", safeFields)
}

func validGraphifyOutcome(outcome string) bool {
	switch outcome {
	case "exec_create_error", "persist_failure", "retry_scheduled", "unresolved_exec",
		"exec_running", "exec_inspect_error", "exec_start_unconfirmed", "exec_succeeded", "exec_failed":
		return true
	default:
		return false
	}
}

func dirtyRepositories(state State) []string {
	repos := []string{}
	for name, value := range state {
		if name == "graphify" {
			continue
		}
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		target, validTarget := numberValue(entry["target"])
		if !validTarget || target == 0 || sameGeneration(entry["target"], entry["completed"]) {
			continue
		}
		repos = append(repos, name)
	}
	sort.Strings(repos)
	return repos
}
func stateEntryMap(state State, name string) map[string]any { return stateMap(state, name) }
func graphAttempt(state State) map[string]any {
	g, ok := state["graphify"].(map[string]any)
	if !ok {
		return nil
	}
	a, _ := g["attempt"].(map[string]any)
	return a
}
func setGraphAttempt(state State, attempt map[string]any) {
	g, ok := state["graphify"].(map[string]any)
	if !ok {
		g = map[string]any{}
		state["graphify"] = g
	}
	if attempt == nil {
		delete(g, "attempt")
	} else {
		g["attempt"] = attempt
	}
}
func attemptSnapshot(attempt map[string]any) []string {
	snapshot, _ := attempt["snapshot"].(map[string]any)
	repos := make([]string, 0, len(snapshot))
	for name := range snapshot {
		repos = append(repos, name)
	}
	sort.Strings(repos)
	return repos
}
func attemptGenerationSnapshot(attempt map[string]any) map[string]any {
	snapshot, _ := attempt["snapshot"].(map[string]any)
	return snapshot
}
func safeRepo(name string) string {
	if safeRepositoryPattern.MatchString(name) {
		return name
	}
	return "unknown"
}
func safeRepos(names []string) []string {
	limit := min(len(names), 100)
	out := make([]string, 0, limit)
	for _, name := range names[:limit] {
		out = append(out, safeRepo(name))
	}
	sort.Strings(out)
	return out
}

func safeDockerStatus(err error) any {
	var api *DockerAPIError
	if errors.As(err, &api) && api.StatusCode >= 100 && api.StatusCode <= 599 {
		return api.StatusCode
	}
	return "unknown"
}
func errorType(err error) string {
	name := fmt.Sprintf("%T", err)
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if !safeErrorTypePattern.MatchString(name) {
		return "unknown"
	}
	return name
}
func numberValue(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case json.Number:
		if integer, err := n.Int64(); err == nil {
			return integer, true
		}
		floating, err := n.Float64()
		if err != nil || floating < math.MinInt64 || floating >= float64(math.MaxInt64) {
			return 0, false
		}
		return int64(floating), true
	default:
		return 0, false
	}
}
func sameGeneration(left, right any) bool {
	leftValue, leftOK := numberValue(left)
	rightValue, rightOK := numberValue(right)
	return leftOK && rightOK && leftValue == rightValue
}
func maxRetryFor(state State, repos []string) int64 {
	max := int64(RetryBaseSeconds)
	for _, repo := range repos {
		n, _ := numberValue(stateEntryMap(state, repo)["failures"])
		delay := RetryDelay(int(n) + 1)
		if delay > max {
			max = delay
		}
	}
	return max
}
func retryDelayRemaining(state State, repos []string, now int64) int64 {
	var max int64
	for _, repo := range repos {
		if due, ok := numberValue(stateEntryMap(state, repo)["retry_due"]); ok && due-now > max {
			max = due - now
		}
	}
	return max
}

func startGate(state State, attempt map[string]any) int64 {
	gate, ok := numberValue(attempt["fallback_deadline"])
	if !ok {
		gate = math.MaxInt64
	}
	snapshot, _ := attempt["snapshot"].(map[string]any)
	for repo := range snapshot {
		due, ok := numberValue(stateEntryMap(state, repo)["retry_due"])
		if ok && due > gate {
			gate = due
		}
	}
	return gate
}
func cloneStateValue(state State) State {
	clone := State{}
	for k, v := range state {
		clone[k] = cloneJSONValue(v)
	}
	return clone
}
func restoreState(state, snapshot State) {
	for key := range state {
		delete(state, key)
	}
	for key, value := range snapshot {
		state[key] = value
	}
}
func safeDiagnostic(value string) string {
	switch value {
	case "workspace_path_missing", "graphify_rebuild_failed", "unclassified", "unavailable":
		return value
	default:
		return "unclassified"
	}
}
