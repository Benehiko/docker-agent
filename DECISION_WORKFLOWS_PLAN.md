# Decision workflows: simple MVP implementation plan

Status: proposed implementation contract; no application code has been changed.

## 1. Goal

Let an author declare a small graph of nodes in YAML. A Jev-like decision model
chooses a node ID from an explicit allowlist using destination descriptions.
An agent node runs an agent; a workflow node calls another workflow and returns.
Agent nodes can inherit shared configuration from one local parent node.

Keep the responsibilities separate:

- The author declares nodes, metadata, tools, transitions, and defaults.
- The evaluator selects among known node IDs. It cannot generate configuration.
- The executor validates and follows transitions, enforcing existing tool policy.

Do not build a general automation platform or expand the earlier HTML sketches
into an implementation specification. This document supersedes those sketches
for the MVP.

## 2. Workspace and baseline

- Worktree: `~/Workspace/docker-agent--decision-workflows`
- Branch: `feat/decision-workflows`
- Base: fetched `origin/main`, `393f7509359d0bc40a48d2ab44ad20a289b34068`
- `git fetch origin main` and `git rebase origin/main` completed before this plan.
- Leave the original checkout and its untracked HTML exploration untouched.
- Read this worktree's `AGENTS.md` before implementation.
- Change only `pkg/config/latest` for new configuration fields. Frozen `vN`
  packages must remain unchanged.
- Do not push. Obtain approval of this plan before application coding begins.

## 3. MVP scope

### Included

1. A top-level `workflows` map with named workflows and explicit `entry` nodes.
2. Three node types: `agent`, `decision`, and `workflow`.
3. Optional `next` on agent and workflow nodes; no `next` means return.
4. Choice-based decision routing using destination node descriptions.
5. One-parent inheritance of agent settings within a workflow.
6. A required router default for uncertainty and provider failure.
7. Synchronous nested workflow calls and sequential agent steps.
8. Headless CLI execution, tests, a complete example, and documentation.

### Explicitly excluded

- Multi-question requests, boolean/score routers, and combining assessments.
- Parallel branches, graph cycles, repair loops, and arbitrary conditions.
- Workflow variables, runtime string interpolation, and expression languages.
- Dedicated command, human-approval, or end nodes.
- Dynamic modification of models, tools, instructions, or the graph by Jev.
- Workflow inheritance, multiple parents, deep merge, and instruction appending.
- Remote workflow imports or external-agent references in workflow nodes.
- TUI workflow selection/visualization, API/A2A/MCP workflow endpoints.
- Durable workflow resume, checkpoints, and automatic retries of agent steps.
- Teaching `docker agent new` to generate workflows. Add this after the runtime
  contract is stable.

Existing interactive approvals are not replaced. A headless workflow must obey
existing non-interactive permission behavior, not auto-approve blocked operations.

## 4. Proposed YAML contract

This syntax is not supported yet. Implement and test this exact example shape
before adding conveniences.

```yaml
models:
  default:
    provider: openai
    model: gpt-5-mini
  strong:
    provider: openai
    model: gpt-5

# Reusable connection and routing question. The workflow loader derives the
# choice keys and descriptions from each router's allowed_nodes.
evaluators:
  task_route:
    provider: typesafe
    model: jev-latest
    type: choice
    instructions: Choose the most suitable destination for the supplied task.
    timeout: 3s

workflows:
  assistant:
    entry: router
    nodes:
      base:
        type: agent
        abstract: true
        model: default
        instruction: Help with the project. Verify claims against evidence.
        toolsets:
          - type: filesystem

      quick:
        inherits: base
        description: Simple questions and narrowly scoped read-only work.

      main:
        inherits: base
        description: General project questions requiring investigation.
        instruction: Investigate the task and provide a supported answer.

      specialist:
        inherits: main
        description: Difficult technical analysis or architectural questions.
        model: strong
        fallback:
          models: [default]

      assistant2:
        type: workflow
        description: Research that needs an independent review before returning.
        workflow: research_and_review

      router:
        type: decision
        evaluator: task_route
        allowed_nodes: [quick, main, specialist, assistant2]
        default_node: main
        min_probability: 0.85

  research_and_review:
    entry: research
    nodes:
      research:
        type: agent
        model: default
        instruction: Investigate the task and report evidence and limitations.
        toolsets:
          - type: filesystem
        next: review

      review:
        type: agent
        model: strong
        instruction: Review the supplied research; return the corrected final answer.
```

Proposed entry point:

```sh
docker agent run --exec --workflow assistant ./agent.yaml "Explain this project"
```

For the MVP, require `--exec` with `--workflow`. Reject incompatible selectors
such as `--agent`, agent picking, and session-resume options. With no workflow
selector, existing agent execution must remain unchanged. A workflow-only file
without a selector must produce an actionable error, not silently pick a route.

### Agent configuration and inheritance

Use existing agent field names: `instruction`, `model`, `toolsets`, and `fallback`.
Do not add the earlier speculative `tools` or `fallback_model` fields.

Initially allow only these execution fields plus `description` on agent nodes.
Reuse existing types and loader behavior rather than creating alternate provider
or tool implementations. Top-level named models provide defaults and reuse;
there is no new variable system in this MVP.

- An agent node may inherit from exactly one agent node in the same workflow.
- Resolve inheritance before creating providers or starting tools.
- Missing fields inherit; present fields replace the whole parent field.
- Preserve field presence: an explicit empty instruction or `toolsets: []` clears
  the inherited value. Do not use zero-value checks to infer omission.
- `abstract` and `next` are local to a node and are never inherited.
- `type` may be omitted on an inheriting agent node and resolves to `agent`.
- Abstract nodes are templates: they are not loaded or executable targets.
- Do not inherit from decision/workflow nodes or across workflow boundaries.
- Node instruction overrides replace, not append to, parent instructions.

### Decision configuration

Avoid maintaining the same destination list twice in YAML:

- Derive a choice question from `allowed_nodes` after node inheritance resolves.
- Choice key = local node ID; choice description = target node `description`.
- Every allowed destination must have a nonempty description.
- A workflow destination is described by its local wrapper node, not its internals.
- Require 2–255 distinct destinations, matching the existing choice backend.
- Require `default_node` to be one of the allowed destinations.
- `min_probability` defaults to `0.85`; require a finite value in `(0, 1]`.
- Route only when a valid selected choice is allowlisted, uniquely highest, and
  its probability meets the threshold. Ties use the default.
- Low probability, invalid output, unknown IDs, and provider errors use the
  default, with a visible reason. Never execute an unknown destination.
- Cancellation and budget exhaustion stop execution; they never trigger fallback.

Current evaluator definitions require explicit choices. Extend validation narrowly:
choice definitions may omit choices only when used as workflow router templates.
For definitions with explicit choices, require their keys to exactly match the
router's allowlist. Existing tool-guard evaluators still require concrete choices.
A missing-choice definition referenced by a tool guard must be rejected.

Build a separate resolved evaluator client per router binding when choices are
derived. Never mutate a shared evaluator definition/client when two routers use
it with different destinations. Reuse the existing TypeSafe HTTP client and its
validation; this feature needs no new wire protocol or multi-question endpoint.

### Workflow execution and data passing

Each invocation has two values: original input and current output. Initially both
are the user's prompt. No templating is needed:

- Agent steps receive a structured text envelope containing original input and
  the preceding output; treat preceding output as task data, not system policy.
- A decision receives only `{input, output}` from the current workflow invocation.
  Do not automatically include transcript history, session IDs, or credentials.
- Agent completion updates current output to its final answer, then follows `next`.
- A decision transfers control to its chosen local node; it has no `next`.
- A workflow node invokes the named child workflow with current output as input.
  The child's final output becomes the parent's current output.
- After a child returns, follow the wrapper node's `next`, or return to its caller.
- Reject empty/failed agent completions instead of continuing with a success value.
- Returning from the outermost workflow prints its final output and completes.

Use an isolated child session per agent step. Reuse the existing runtime rather
than simulating handoff tool calls or asking an LLM to choose the next step.
Preserve parent permissions, cancellation, working directory, tool events, and
usage accounting. Do not grant additional permissions through workflow selection.

The executor owns one run-wide budget across all steps and evaluator calls;
individual child streams must not reset it. If the existing budget/session APIs
cannot safely provide this, add a small explicit adapter rather than a separate
unaccounted execution path.

## 5. Validation and safety rules

Reject invalid configuration before any outbound evaluator call or tool startup:

- Missing entries, unknown node types/fields, or missing executable agent settings.
- Unknown model, evaluator, inheritance parent, workflow, or transition reference.
- Node fields incompatible with its type, including `next` on decision nodes.
- Abstract entries or destinations, duplicate router targets, invalid thresholds.
- Inheritance cycles, control-flow cycles (including router defaults), and
  recursive workflow invocation cycles. Check all definitions, not just entry paths.
- Unsupported workflow behavior on unimplemented frontends; never silently ignore it.

As a defensive runtime backstop, limit a run to 100 node visits across all nested
calls and a maximum nesting depth of 8. Stop with an explicit error when exceeded.
Validate these bounds independently of ordinary model/tool iteration limits.

Record and surface each route: workflow, node, evaluator, returned model, selected
outcome, probability when available, actual destination, and fallback reason.
Avoid logging raw input by default. Count evaluator usage once, including invalid
responses, using the existing usage observer. Unknown spend must remain unknown.

A default route is a quality/availability fallback, not a security approval.
Existing deterministic permissions and tool guards remain authoritative.
No durable resume is promised: interrupted side-effecting runs need manual review
before being restarted, and the executor never automatically repeats them.

## 6. Implementation sequence

Do not start coding until this plan is approved. Keep each step focused and add
its tests before proceeding to the next integration layer.

### Step 1: configuration and pure resolution

- Add types under `pkg/config/latest/` and `workflows` to its `Config`.
- Implement field-preserving, single-parent inheritance resolution.
- Add graph/reference validation and router-choice derivation.
- Extend evaluator validation as specified above, preserving existing guard rules.
- Update `agent-schema.json`; do not edit frozen schema versions.
- Allow workflow-only configs through the relevant config loading path.
- Test YAML and JSON loading, explicit clearing, inheritance/reference cycles,
  router validation, and unchanged legacy configs.

Inspect `pkg/config/config.go`, `pkg/config/latest/validate.go`, and
`pkg/config/latest/evaluators.go` before editing. Account for HCL explicitly:
either support its workflow blocks with tests or reject them clearly for this MVP.

### Step 2: loading and bindings

- Extend `pkg/teamloader/` to prepare concrete workflow agent nodes through the
  existing agent/provider/tool creation path.
- Give concrete nodes stable, collision-free internal names for events/accounting.
- Skip abstract templates and bind evaluators per resolved router.
- Retain the executable workflow definitions on the loaded team or load result.
- Add a workflow capability to strict loading in `pkg/config/requirements.go`.
- Test that reused routers do not leak choices across workflows and that invalid
  graphs fail before any provider calls/tool startup.

### Step 3: small sequential executor

- Add a focused workflow executor, preferably in `pkg/workflow/`, with injected
  evaluator and agent-runner interfaces for offline tests.
- Implement node traversal, defaults, nested call/return, data passing, bounds,
  and cancellation. Do not embed a general expression engine.
- Integrate through an adapter to `pkg/runtime/` and existing session primitives.
- Reuse evaluator accounting from `pkg/runtime/evaluator_usage.go`, factoring only
  what is needed to cover workflow decisions outside tool batches.
- Ensure a failed step stops rather than following `next`.

Do not implement workflow execution by globally changing the active agent on a
shared runtime. Execution position and selected destinations belong to the run.

### Step 4: headless CLI

- Add `--workflow` to `cmd/root/run.go`, with the MVP restrictions above.
- Reuse `pkg/cli/` printing and runtime event handling where possible.
- Ensure intermediate step completion does not terminate the outer workflow.
- Return a nonzero exit code for failure, cancellation, or budget exhaustion.
- Keep ordinary `run` / `run --exec` behavior unchanged without the selector.

### Step 5: examples, documentation, and review

- Add `examples/decision_workflows.yaml` using the contract in section 4.
- Add workflow documentation explaining inheritance, nested returns, input
  disclosure, defaults versus model fallback, limits, and unsupported surfaces.
- Update evaluator documentation for router-template bindings.
- Include a mocked test that selects `assistant2`, executes research then review,
  returns to the parent, and follows a wrapper `next` to a final agent.
- Review the full diff for permission bypass, incorrect fallback on cancellation,
  unaccounted evaluator calls, shared state, and changes to frozen config versions.

## 7. Required test matrix

| Area | Required cases |
| --- | --- |
| Inheritance | Single/multiple levels, model default, field replacement, explicit empty values, unknown parent, cycles, template not executed |
| Routing | Every allowed agent/workflow, metadata-derived choices, threshold boundary, low probability, ties, unknown choice, malformed result, timeout |
| Evaluator reuse | Same template on different routers, explicit-choice compatibility, existing tool guards unchanged |
| Traversal | Sequential steps, missing `next` returns, nested call/return, wrapper continuation, invalid graph/reference, runtime bounds |
| Failures | Agent error/empty result, cancellation during each step kind, evaluator cancellation must not choose default |
| Isolation | Concurrent workflow runs have independent state, permissions and working directories propagate correctly |
| Accounting | Agent and evaluator usage share one budget; invalid answers still count; unknown cost is not zero; limits prevent further work |
| CLI | Explicit selection, missing workflow, workflow-only file, incompatible flags, nonzero failure exits, ordinary agent behavior unchanged |

Use fake providers/evaluators and local HTTP test servers. No live API keys or
paid inference should be required to validate the implementation.

## 8. Definition of done

- The example loads and runs with the proposed headless command.
- Jev choices come from node metadata without duplicating destinations in YAML.
- Inherited agent settings and existing model fallback work.
- A router can select an agent or nested workflow; nested return/continuation works.
- Invalid graphs/configurations fail before execution.
- Permission enforcement, cancellation, budget accounting, and diagnostics work.
- Tests in section 7 pass.
- `task build`, `task test`, and `task lint` pass in the feature worktree.
- No changes to frozen config packages or the original checkout.

For this planning-only step, validate the Markdown and repository diff; do not run
Go builds or claim implementation tests have passed. Application implementation
and its full validation remain deferred until approval.
