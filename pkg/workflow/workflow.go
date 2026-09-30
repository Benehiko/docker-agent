// Package workflow executes validated, sequential decision graphs.
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
)

const (
	maxVisits = 100
	maxDepth  = 8
)

// AgentRunner runs a single isolated agent step with task data.
type AgentRunner interface {
	RunAgent(ctx context.Context, workflow, node, input, output string) (string, error)
}

// Route records the model's choice and the actual, validated destination.
type Route struct {
	Workflow, Node, Evaluator, Model, Selected, Destination, FallbackReason string
	Probability                                                             *float64
}

// Executor runs one invocation; its configuration and bindings are read-only and safe to share.
type Executor struct {
	Workflows    map[string]latest.WorkflowConfig
	Routers      map[string]map[string]evaluator.Evaluator
	Agents       AgentRunner
	OnRoute      func(Route)
	Check        func(context.Context) error
	Conversation string
}

func (e *Executor) Run(ctx context.Context, name, input string) (string, error) {
	if _, ok := e.Workflows[name]; !ok {
		return "", fmt.Errorf("workflow %q not found", name)
	}
	visits := 0
	return e.run(ctx, name, input, 1, &visits)
}

func (e *Executor) run(ctx context.Context, name, input string, depth int, visits *int) (string, error) {
	if depth > maxDepth {
		return "", fmt.Errorf("workflow nesting exceeds %d", maxDepth)
	}
	wf := e.Workflows[name]
	output := input
	for id := wf.Entry; id != ""; {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if e.Check != nil {
			if err := e.Check(ctx); err != nil {
				return "", err
			}
		}
		*visits++
		if *visits > maxVisits {
			return "", fmt.Errorf("workflow node visits exceed %d", maxVisits)
		}
		node, ok := wf.Nodes[id]
		if !ok || node.Abstract {
			return "", fmt.Errorf("workflow %q has invalid node %q", name, id)
		}
		switch node.Type {
		case "agent":
			if e.Agents == nil {
				return "", errors.New("workflow agent runner is missing")
			}
			answer, err := e.Agents.RunAgent(ctx, name, id, input, output)
			if err != nil {
				return "", fmt.Errorf("workflow %s node %s: %w", name, id, err)
			}
			if strings.TrimSpace(answer) == "" {
				return "", fmt.Errorf("workflow %s node %s: empty agent completion", name, id)
			}
			output, id = answer, node.Next
		case "workflow":
			answer, err := e.run(ctx, node.Workflow, output, depth+1, visits)
			if err != nil {
				return "", err
			}
			output, id = answer, node.Next
		case "decision":
			client := e.Routers[name][id]
			if client == nil {
				return "", fmt.Errorf("workflow %s node %s: missing evaluator binding", name, id)
			}
			state := map[string]string{"input": input, "output": output}
			if e.Conversation != "" {
				state["conversation"] = e.Conversation
			}
			result, err := client.Evaluate(ctx, state)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
				return "", err
			}
			if _, ok := errors.AsType[*runtime.WorkflowBudgetError](err); ok {
				return "", err
			}
			route := choose(name, id, node, result, err)
			probability := any(nil)
			if route.Probability != nil {
				probability = *route.Probability
			}
			slog.InfoContext(ctx, "Workflow route", "workflow", name, "node", id, "evaluator", node.Evaluator, "model", route.Model, "selected", route.Selected, "probability", probability, "destination", route.Destination, "fallback_reason", route.FallbackReason)
			if e.OnRoute != nil {
				e.OnRoute(route)
			}
			id = route.Destination
		default:
			return "", fmt.Errorf("workflow %s node %s: unknown type %q", name, id, node.Type)
		}
	}
	return output, nil
}

// RuntimeAgentRunner connects the executor to isolated local runtime sessions.
type RuntimeAgentRunner struct {
	Runtime      *runtime.LocalRuntime
	Session      *session.Session
	Events       runtime.EventSink
	Conversation string
}

func (a RuntimeAgentRunner) RunAgent(ctx context.Context, workflow, node, input, output string) (string, error) {
	prompt := fmt.Sprintf("<original_input>\n%s\n</original_input>\n\n<previous_output>\n%s\n</previous_output>", input, output)
	if a.Conversation != "" {
		prompt += "\n\n<conversation>\n" + a.Conversation + "\n</conversation>"
	}
	return a.Runtime.RunWorkflowStep(ctx, a.Session, teamloader.WorkflowAgentName(workflow, node), prompt, a.Events)
}

// RuntimeEvaluator uses the runtime's existing evaluator usage observer and budget.
type RuntimeEvaluator struct {
	Runtime   *runtime.LocalRuntime
	Session   *session.Session
	AgentName string
	Name      string
	Client    evaluator.Evaluator
	Events    runtime.EventSink
}

func (e RuntimeEvaluator) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	return e.Runtime.EvaluateWorkflow(ctx, e.Session, e.AgentName, e.Name, e.Client, state, e.Events)
}

func choose(name, id string, node latest.WorkflowNode, result *evaluator.Result, err error) Route {
	r := Route{Workflow: name, Node: id, Evaluator: node.Evaluator, Destination: node.DefaultNode}
	switch {
	case err != nil:
		r.FallbackReason = err.Error()
	case result == nil || result.Type != "choice":
		r.FallbackReason = "invalid evaluator result"
	default:
		r.Model, r.Selected = result.Model, result.Choice
		p, ok := result.Probabilities[result.Choice]
		if ok && !math.IsNaN(p) && !math.IsInf(p, 0) {
			r.Probability = &p
		}
		allowed := false
		for _, target := range node.AllowedNodes {
			if target == result.Choice {
				allowed = true
			}
		}
		valid := len(result.Probabilities) == len(node.AllowedNodes)
		var total float64
		for _, target := range node.AllowedNodes {
			value, present := result.Probabilities[target]
			if !present || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
				valid = false
			}
			total += value
		}
		valid = valid && math.Abs(total-1) <= 1e-3
		threshold := 0.85
		if node.MinProbability != nil {
			threshold = *node.MinProbability
		}
		switch {
		case !allowed || !ok || r.Probability == nil || !valid:
			r.FallbackReason = "invalid or unknown choice"
		default:
			for target, other := range result.Probabilities {
				if target != result.Choice && other >= p {
					r.FallbackReason = "choice is not uniquely highest"
					break
				}
			}
			if r.FallbackReason == "" {
				if p < threshold {
					r.FallbackReason = "probability below threshold"
				} else {
					r.Destination = result.Choice
				}
			}
		}
	}
	return r
}

// Runner binds a decision graph to the native runtime's root stream lifecycle.
func Runner(name string, workflows map[string]latest.WorkflowConfig, routers map[string]map[string]evaluator.Evaluator) runtime.WorkflowRunner {
	return func(ctx context.Context, rt *runtime.LocalRuntime, sess *session.Session, events runtime.EventSink) (string, error) {
		input, conversation := conversationInput(sess)
		executor := Executor{
			Workflows: workflows, Conversation: conversation,
			Routers: make(map[string]map[string]evaluator.Evaluator),
			Agents:  RuntimeAgentRunner{Runtime: rt, Session: sess, Events: events, Conversation: conversation},
			Check:   func(context.Context) error { return rt.WorkflowBudget(rt.CurrentAgentName(ctx)) },
			OnRoute: func(route Route) {
				if route.FallbackReason != "" {
					events.Emit(runtime.Warning(fmt.Sprintf("workflow %s/%s routed to %s: %s", route.Workflow, route.Node, route.Destination, route.FallbackReason), rt.CurrentAgentName(ctx)))
				}
			},
		}
		for workflowName, nodes := range routers {
			executor.Routers[workflowName] = make(map[string]evaluator.Evaluator)
			for id, client := range nodes {
				executor.Routers[workflowName][id] = RuntimeEvaluator{Runtime: rt, Session: sess, AgentName: rt.CurrentAgentName(ctx), Name: workflowName + "/" + id, Client: client, Events: events}
			}
		}
		return executor.Run(ctx, name, input)
	}
}

// Only visible conversation is disclosed to routers, never tool transcripts or system instructions.
func conversationInput(sess *session.Session) (string, string) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var messages []message
	lastUser := -1
	for _, item := range sess.MessagesSnapshot() {
		if !item.IsMessage() || (item.Message.Implicit && item.Message.Message.Role != chat.MessageRoleAssistant) {
			continue
		}
		msg := item.Message.Message
		if msg.Role == chat.MessageRoleUser || msg.Role == chat.MessageRoleAssistant {
			if msg.Role == chat.MessageRoleUser {
				lastUser = len(messages)
			}
			messages = append(messages, message{Role: string(msg.Role), Content: msg.Content})
		}
	}
	if lastUser < 0 {
		return "", ""
	}
	input := messages[lastUser].Content
	if lastUser == 0 {
		return input, ""
	}
	// Stop at the current input so retry never sends the previous failed attempt.
	data, _ := json.Marshal(messages[:lastUser]) // String-only fields cannot fail to marshal.
	return input, string(data)
}
