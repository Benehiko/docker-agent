package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// ResolveWorkflowToolsets resolves inherited toolset references without changing
// the author definitions. Each concrete agent gets its own copy of the toolsets.
func ResolveWorkflowToolsets(cfg *latest.Config) (map[string]latest.WorkflowConfig, error) {
	workflows, err := cfg.ResolvedWorkflows()
	if err != nil {
		return nil, err
	}
	for wfName, wf := range workflows {
		for id, node := range wf.Nodes {
			if node.Type != "agent" || node.Abstract || node.Toolsets == nil {
				continue
			}
			copied := slices.Clone(*node.Toolsets)
			for i := range copied {
				ts := &copied[i]
				switch ts.Type {
				case "mcp":
					if ts.Ref == "" || strings.HasPrefix(ts.Ref, "docker:") {
						continue
					}
					def, ok := cfg.MCPs[ts.Ref]
					if !ok {
						return nil, fmt.Errorf("workflows.%s.nodes.%s: unknown MCP definition %q", wfName, id, ts.Ref)
					}
					applyMCPDefaults(ts, &def.Toolset)
				case "rag":
					if ts.Ref == "" {
						continue
					}
					ref := ts.Ref
					def, ok := cfg.RAG[ref]
					if !ok {
						return nil, fmt.Errorf("workflows.%s.nodes.%s: unknown RAG definition %q", wfName, id, ref)
					}
					applyRAGDefaults(ts, &def.Toolset, ref)
				}
			}
			node.Toolsets = &copied
			wf.Nodes[id] = node
		}
		workflows[wfName] = wf
	}
	return workflows, nil
}
