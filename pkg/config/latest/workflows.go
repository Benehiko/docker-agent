package latest

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// WorkflowConfig defines a local, acyclic decision graph.
type WorkflowConfig struct {
	Entry string                  `json:"entry"`
	Nodes map[string]WorkflowNode `json:"nodes"`
}

// WorkflowNode is a node in a workflow. Pointer fields preserve omission during inheritance.
type WorkflowNode struct {
	Type           string          `json:"type,omitempty"`
	Description    string          `json:"description,omitempty"`
	Inherits       string          `json:"inherits,omitempty"`
	Abstract       bool            `json:"abstract,omitempty"`
	Model          *string         `json:"model,omitempty"`
	Instruction    *string         `json:"instruction,omitempty"`
	Toolsets       *[]Toolset      `json:"toolsets,omitempty"`
	Fallback       *FallbackConfig `json:"fallback,omitempty"`
	Next           string          `json:"next,omitempty"`
	Workflow       string          `json:"workflow,omitempty"`
	Evaluator      string          `json:"evaluator,omitempty"`
	AllowedNodes   []string        `json:"allowed_nodes,omitempty"`
	DefaultNode    string          `json:"default_node,omitempty"`
	MinProbability *float64        `json:"min_probability,omitempty"`
}

// UnmarshalYAML preserves explicit empty lists, which the generic YAML decoder
// otherwise treats the same as an omitted slice pointer.
func (n *WorkflowNode) UnmarshalYAML(decode func(any) error) error {
	type plain WorkflowNode
	var parsed plain
	if err := decode(&parsed); err != nil {
		return err
	}
	*n = WorkflowNode(parsed)
	var fields map[string]any
	if err := decode(&fields); err != nil {
		return err
	}
	if _, present := fields["toolsets"]; present && n.Toolsets == nil {
		empty := []Toolset{}
		n.Toolsets = &empty
	}
	return nil
}

// ResolvedWorkflows validates and resolves every definition without mutating the config.
func (t *Config) ResolvedWorkflows() (map[string]WorkflowConfig, error) {
	resolved := make(map[string]WorkflowConfig, len(t.Workflows))
	for name, wf := range t.Workflows {
		if strings.TrimSpace(name) == "" || wf.Entry == "" || len(wf.Nodes) == 0 {
			return nil, fmt.Errorf("workflows.%s: name, entry, and nodes are required", name)
		}
		out := WorkflowConfig{Entry: wf.Entry, Nodes: make(map[string]WorkflowNode, len(wf.Nodes))}
		visiting := make(map[string]bool)
		var resolve func(string) (WorkflowNode, error)
		resolve = func(id string) (WorkflowNode, error) {
			if n, ok := out.Nodes[id]; ok {
				return n, nil
			}
			n, ok := wf.Nodes[id]
			if !ok {
				return n, fmt.Errorf("unknown node %q", id)
			}
			if visiting[id] {
				return n, fmt.Errorf("inheritance cycle at %q", id)
			}
			visiting[id] = true
			defer delete(visiting, id)
			if n.Inherits != "" {
				parent, err := resolve(n.Inherits)
				if err != nil {
					return n, err
				}
				if parent.Type != "agent" {
					return n, fmt.Errorf("node %q must inherit an agent", id)
				}
				if n.Type != "" && n.Type != "agent" {
					return n, fmt.Errorf("node %q: only agents may inherit", id)
				}
				n.Type = "agent"
				if n.Model == nil {
					n.Model = parent.Model
				}
				if n.Instruction == nil {
					n.Instruction = parent.Instruction
				}
				if n.Toolsets == nil {
					n.Toolsets = parent.Toolsets
				}
				if n.Fallback == nil {
					n.Fallback = parent.Fallback
				}
			}
			if err := t.validateWorkflowNode(id, n); err != nil {
				return n, err
			}
			out.Nodes[id] = n
			return n, nil
		}
		for id := range wf.Nodes {
			if strings.TrimSpace(id) == "" {
				return nil, fmt.Errorf("workflows.%s: node IDs must not be empty", name)
			}
			if _, err := resolve(id); err != nil {
				return nil, fmt.Errorf("workflows.%s: %w", name, err)
			}
		}
		entry, ok := out.Nodes[wf.Entry]
		if !ok || entry.Abstract {
			return nil, fmt.Errorf("workflows.%s: entry %q must be executable", name, wf.Entry)
		}
		for id, n := range out.Nodes {
			if n.Next != "" {
				target, ok := out.Nodes[n.Next]
				if !ok || target.Abstract {
					return nil, fmt.Errorf("workflows.%s.nodes.%s: invalid next %q", name, id, n.Next)
				}
			}
			if n.Type == "decision" {
				if err := validateRouter(t, out.Nodes, n); err != nil {
					return nil, fmt.Errorf("workflows.%s.nodes.%s: %w", name, id, err)
				}
			}
		}
		resolved[name] = out
	}
	for name, wf := range resolved {
		for id, n := range wf.Nodes {
			if n.Type == "workflow" {
				if _, ok := resolved[n.Workflow]; !ok {
					return nil, fmt.Errorf("workflows.%s.nodes.%s: unknown workflow %q", name, id, n.Workflow)
				}
			}
		}
		state := map[string]int{}
		var visit func(string) error
		visit = func(id string) error {
			if state[id] == 1 {
				return fmt.Errorf("control-flow cycle at %q", id)
			}
			if state[id] == 2 {
				return nil
			}
			state[id] = 1
			n := wf.Nodes[id]
			targets := []string{n.Next}
			if n.Type == "decision" {
				targets = n.AllowedNodes
			}
			for _, target := range targets {
				if target != "" {
					if err := visit(target); err != nil {
						return err
					}
				}
			}
			state[id] = 2
			return nil
		}
		for id := range wf.Nodes {
			if err := visit(id); err != nil {
				return nil, fmt.Errorf("workflows.%s: %w", name, err)
			}
		}
	}
	state := map[string]int{}
	var visitWorkflow func(string) error
	visitWorkflow = func(name string) error {
		if state[name] == 1 {
			return fmt.Errorf("recursive workflow call at %q", name)
		}
		if state[name] == 2 {
			return nil
		}
		state[name] = 1
		for _, n := range resolved[name].Nodes {
			if n.Type == "workflow" {
				if err := visitWorkflow(n.Workflow); err != nil {
					return err
				}
			}
		}
		state[name] = 2
		return nil
	}
	for name := range resolved {
		if err := visitWorkflow(name); err != nil {
			return nil, err
		}
	}
	return resolved, nil
}

func (t *Config) validateWorkflowNode(id string, n WorkflowNode) error {
	bad := func(fields string) error { return fmt.Errorf("node %q: incompatible %s for %s", id, fields, n.Type) }
	switch n.Type {
	case "agent":
		if n.Workflow != "" || n.Evaluator != "" || n.AllowedNodes != nil || n.DefaultNode != "" || n.MinProbability != nil {
			return bad("routing fields")
		}
		if !n.Abstract && (n.Model == nil || *n.Model == "" || n.Instruction == nil) {
			return fmt.Errorf("node %q: executable agent needs model and instruction", id)
		}
		if n.Model != nil {
			if _, ok := t.Models[*n.Model]; !ok && *n.Model != "auto" {
				if _, err := ParseModelRef(*n.Model); err != nil {
					return fmt.Errorf("node %q: unknown model %q", id, *n.Model)
				}
			}
		}
		if n.Fallback != nil {
			if err := (&AgentConfig{Fallback: n.Fallback}).validateFallback(); err != nil {
				return err
			}
			for _, ref := range n.Fallback.Models {
				if _, ok := t.Models[ref]; !ok {
					if _, err := ParseModelRef(ref); err != nil {
						return fmt.Errorf("node %q: unknown fallback model %q", id, ref)
					}
				}
			}
		}
		if n.Toolsets != nil {
			for _, ts := range *n.Toolsets {
				if err := ts.validate(); err != nil {
					return fmt.Errorf("node %q: %w", id, err)
				}
			}
		}
	case "decision":
		if n.Inherits != "" || n.Abstract || n.Model != nil || n.Instruction != nil || n.Toolsets != nil || n.Fallback != nil || n.Next != "" || n.Workflow != "" {
			return bad("agent/workflow fields")
		}
	case "workflow":
		if n.Inherits != "" || n.Abstract || n.Model != nil || n.Instruction != nil || n.Toolsets != nil || n.Fallback != nil || n.Evaluator != "" || n.AllowedNodes != nil || n.DefaultNode != "" || n.MinProbability != nil {
			return bad("agent/decision fields")
		}
		if n.Workflow == "" {
			return fmt.Errorf("node %q: workflow reference is required", id)
		}
	default:
		return fmt.Errorf("node %q: unknown type %q", id, n.Type)
	}
	return nil
}

func validateRouter(c *Config, nodes map[string]WorkflowNode, n WorkflowNode) error {
	def, ok := c.Evaluators[n.Evaluator]
	if !ok || def.Type != "choice" {
		return fmt.Errorf("unknown choice evaluator %q", n.Evaluator)
	}
	if len(n.AllowedNodes) < 2 || len(n.AllowedNodes) > 255 {
		return errors.New("allowed_nodes must contain 2-255 destinations")
	}
	seen := map[string]bool{}
	for _, id := range n.AllowedNodes {
		target, ok := nodes[id]
		if !ok || target.Abstract || target.Type == "decision" || strings.TrimSpace(target.Description) == "" || seen[id] {
			return fmt.Errorf("invalid or duplicate destination %q (requires a described executable agent or workflow)", id)
		}
		seen[id] = true
	}
	if !seen[n.DefaultNode] {
		return errors.New("default_node must be one of allowed_nodes")
	}
	if n.MinProbability != nil && (math.IsNaN(*n.MinProbability) || math.IsInf(*n.MinProbability, 0) || *n.MinProbability <= 0 || *n.MinProbability > 1) {
		return errors.New("min_probability must be finite and in (0, 1]")
	}
	if len(def.Choices) > 0 {
		if len(def.Choices) != len(seen) {
			return errors.New("evaluator choices must exactly match allowed_nodes")
		}
		for id := range def.Choices {
			if !seen[id] {
				return fmt.Errorf("evaluator choice %q is not allowed", id)
			}
		}
	}
	return nil
}

// RouterEvaluator returns an independent evaluator definition with choices from node descriptions.
func (t *Config) RouterEvaluator(wf WorkflowConfig, node WorkflowNode) EvaluatorConfig {
	def := t.Evaluators[node.Evaluator]
	def.Choices = make(map[string]string, len(node.AllowedNodes))
	for _, id := range node.AllowedNodes {
		def.Choices[id] = wf.Nodes[id].Description
	}
	return def
}

func (n WorkflowNode) AgentConfig(name string) AgentConfig {
	a := AgentConfig{Name: name, Description: n.Description, Fallback: n.Fallback}
	if n.Model != nil {
		a.Model = *n.Model
	}
	if n.Instruction != nil {
		a.Instruction = *n.Instruction
	}
	if n.Toolsets != nil {
		a.Toolsets = slices.Clone(*n.Toolsets)
	}
	return a
}
