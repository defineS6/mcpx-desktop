package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// updateWorkspaces 按名称和路径匹配原节点，仅更新变化字段并保留单条记录的未知字段。
func updateWorkspaces(root *yaml.Node, workspaces []WorkspaceEntry) error {
	previous := lookup(root, "workspaces")
	var oldNodes []*yaml.Node
	if previous != nil && previous.Tag != "!!null" {
		if previous.Kind != yaml.SequenceNode {
			return fmt.Errorf("workspaces 必须是列表")
		}
		oldNodes = previous.Content
	}
	oldEntries := make([]WorkspaceEntry, len(oldNodes))
	for i, node := range oldNodes {
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("workspaces 第 %d 条必须是映射", i+1)
		}
		if err := node.Decode(&oldEntries[i]); err != nil {
			return fmt.Errorf("解析工作区第 %d 条: %w", i+1, err)
		}
	}
	// 先全局匹配名称与路径同时相同的记录，再按名称、路径匹配，避免重排抢走原节点。
	matched := make([]int, len(workspaces))
	used := make([]bool, len(oldNodes))
	for i := range matched {
		matched[i] = -1
	}
	for pass := 0; pass < 3; pass++ {
		for i, entry := range workspaces {
			if matched[i] >= 0 {
				continue
			}
			for j, old := range oldEntries {
				if used[j] {
					continue
				}
				match := (pass == 0 && old.Name == entry.Name && old.Path == entry.Path) ||
					(pass == 1 && old.Name == entry.Name) || (pass == 2 && old.Path == entry.Path)
				if match {
					matched[i], used[j] = j, true
					break
				}
			}
		}
	}
	next := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	if previous != nil {
		copy := *previous
		next = &copy
		next.Kind, next.Tag, next.Value, next.Alias, next.Style = yaml.SequenceNode, "!!seq", "", nil, 0
		next.Content = nil
	}
	for i, entry := range workspaces {
		var node *yaml.Node
		if matched[i] < 0 {
			node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			put(node, []string{"name"}, entry.Name)
			put(node, []string{"path"}, entry.Path)
			if entry.Description != "" {
				put(node, []string{"description"}, entry.Description)
			}
		} else {
			node = oldNodes[matched[i]]
			before := oldEntries[matched[i]]
			if before.Name != entry.Name {
				put(node, []string{"name"}, entry.Name)
			}
			if before.Path != entry.Path {
				put(node, []string{"path"}, entry.Path)
			}
			if before.Description != entry.Description {
				put(node, []string{"description"}, entry.Description)
			}
		}
		next.Content = append(next.Content, node)
	}
	setChild(root, "workspaces", next)
	return nil
}
