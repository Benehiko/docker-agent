---
title: "Decision workflows"
description: "Route among declared agent steps and nested workflows with a choice evaluator."
weight: 66
---

Decision workflows are available in the latest configuration format. See the
[complete example](https://github.com/docker/docker-agent/blob/main/examples/decision_workflows.yaml).
Run one in the interactive TUI:

```sh
docker agent run --workflow assistant ./examples/decision_workflows.yaml
```

If a file defines exactly one workflow, `docker agent run ./config.yaml`
selects it automatically, even if the file also contains an ordinary root
agent. Use `--agent` to explicitly run an ordinary agent instead. With multiple
workflows, select one with `--workflow`; workflow-only files never fall back to
running an internal node as an ordinary agent.

The TUI streams node output and tool activity, supports interactive approvals
and elicitation, and cancels the entire workflow with Escape. Every subsequent
message runs the graph again with conversation context. Input submitted while
busy is processed at the next workflow boundary, not injected into a selected
node.

Headless execution remains available with `--exec --workflow <name>` and one
prompt (or `-` to read stdin). Workflow runs cannot be combined with `--agent`,
`--agent-picker`, session resume, `--remote`, `--listen`, attachments, or
`--json`. HCL, API, A2A, and MCP workflow entry points are not supported.

A `workflows` map names independent graphs. Each graph has an `entry` and
`nodes`; a node has one of these types:

- `agent`: Runs an isolated agent with the current task and returns its final
  answer. `next` continues to another local node; omitting `next` returns.
- `decision`: Uses a named choice evaluator, `allowed_nodes`, and a required
  `default_node` to choose a described local `agent` or `workflow` node. No
  `next` is allowed.
- `workflow`: Invokes another named workflow with the current output as its
  input. The child returns its final output; the wrapper's `next` continues
  in the caller, or omission returns.

An agent node can inherit from **one agent node in the same workflow** with
`inherits: parent`. Missing `model`, `instruction`, `toolsets`, and `fallback`
fields inherit; present values replace the complete parent value. For example,
`instruction: ""` and `toolsets: []` explicitly clear inherited values. An
`abstract: true` agent is a template and cannot be an entry or route target.
`next` and `abstract` never inherit. Model `fallback` selects a replacement
*chat model* on agent failure; `default_node` selects a *route* when assessment
is uncertain or unavailable. They are unrelated.

The choice evaluator's choices are derived from allowed local node IDs and
those nodes' `description` fields. Supply two to 255 distinct described
executable destinations; if an evaluator declares `choices` explicitly, their
keys must exactly match the router allowlist. `min_probability` defaults to
`0.85`. An unknown or invalid answer, tie, low probability, or provider error
routes to `default_node` and emits a reason. Cancellation and exhausted budgets
stop instead. Routes record workflow, node, evaluator, returned model, selected
outcome and probability, actual destination, and fallback reason; raw task input
is not logged by default.

Each workflow invocation tracks its original `input` and current `output`.
In the TUI, follow-up decisions also disclose a `conversation` field containing
prior user messages and final workflow answers. System instructions, tool
transcripts, and intermediate node output from earlier turns are excluded.
Each agent receives this context and both values in a task-data envelope; preceding
agent output is untrusted content, not system policy. A child workflow receives
the parent's current output as its own input. Agents use separate sessions;
tool permissions, session policy, working directory, and the run-wide budget
propagate to each step. In headless execution, operations that require an
interactive confirmation are rejected rather than silently approved. In the
TUI, confirmations use the normal approval dialog. Session-wide approval
choices carry forward to later steps and turns; selecting a workflow never
automatically approves tools or relaxes the configured safety mode. Tool
guards and explicit permission denials remain authoritative. The executor
stops on empty agent completions, errors, cancellation, or budget exhaustion.

All graphs must be acyclic, including nested workflow calls. The runtime also
limits an invocation to 100 total node visits and 8 levels of nesting. There
are no automatic retries of agent steps or durable checkpoints. After an
interrupted run with side effects, inspect the workspace manually before
restarting. Variables, interpolation, conditions, parallel branches, approval
nodes, and remote workflow imports are outside this MVP.
