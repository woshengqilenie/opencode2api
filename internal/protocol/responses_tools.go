package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"opencode2api/internal/jsonutil"
)

// Codex (and the wider Responses API) declares more than plain function tools:
//
//   - "namespace" bundles nested functions (multi_agent_v1 → close_agent, ...);
//   - "custom" tools accept free-form text (apply_patch and friends);
//   - provider-side tools such as web_search have no chat-completions
//     equivalent at all.
//
// The old decoder rejected anything that was not a function tool, which made
// every Codex CLI session fail with "tools[7] must be a function tool". The
// gateway now converts what it can and drops the rest, so the remaining toolset
// stays usable. Namespaced tools are flattened to "<namespace>__<tool>" (the
// same layout CC Switch uses, so behaviour matches what Codex users see behind
// other relays) and the alias table maps every chat-visible name back to the
// Responses tool the client actually declared.
const (
	chatToolNameMaxLen         = 64
	customToolInputField       = "input"
	customToolInputDescription = "Raw string input for the original custom tool. Preserve formatting exactly and follow the original tool definition embedded in the description."
	customToolMetadataHeading  = "Custom tool definition (preserved):"
)

// ToolAliasKind classifies how a chat-visible tool maps back to what the
// client declared.
type ToolAliasKind uint8

const (
	// ToolAliasFunction is a plain function tool.
	ToolAliasFunction ToolAliasKind = iota
	// ToolAliasNamespace is a function tool nested inside a namespace tool.
	ToolAliasNamespace
	// ToolAliasCustom is a free-form custom tool exposed as a one-string
	// function on the chat wire.
	ToolAliasCustom
)

// ToolAlias records the Responses-side identity of a chat tool name.
type ToolAlias struct {
	Kind      ToolAliasKind
	Name      string // original tool name (the child name for namespaced tools)
	Namespace string // namespace for ToolAliasNamespace
}

// ToolAliases maps chat tool names to the Responses tools they came from.
// A nil *ToolAliases is valid and behaves like an empty table.
type ToolAliases struct {
	byChatName map[string]ToolAlias
}

// Lookup returns the alias recorded for a chat tool name.
func (aliases *ToolAliases) Lookup(chatName string) (ToolAlias, bool) {
	if aliases == nil {
		return ToolAlias{}, false
	}
	alias, ok := aliases.byChatName[chatName]
	return alias, ok
}

// Len reports how many aliases are recorded.
func (aliases *ToolAliases) Len() int {
	if aliases == nil {
		return 0
	}
	return len(aliases.byChatName)
}

// BuildResponsesAliases derives the alias table from a Responses request body.
// The gateway uses it to translate upstream tool calls back into the exact item
// shapes the downstream client declared.
func BuildResponsesAliases(payload map[string]any) *ToolAliases {
	_, aliases := ResponsesToolsToChat(payload)
	return aliases
}

// ResponsesToolsToChat converts the tools declared on a Responses request into
// chat tool definitions plus the alias table needed to map calls back.
func ResponsesToolsToChat(payload map[string]any) ([]bridgeTool, *ToolAliases) {
	aliases := &ToolAliases{byChatName: map[string]ToolAlias{}}
	tools := make([]bridgeTool, 0, 8)
	for _, raw := range jsonutil.SliceAt(payload, "tools") {
		switch tool := raw.(type) {
		case string:
			// A bare string names a custom tool.
			appendCustomTool(&tools, tool, map[string]any{"type": "custom", "name": tool}, aliases)
		case map[string]any:
			switch jsonutil.StringAt(tool, "type") {
			case "function":
				appendFunctionTool(&tools, tool, "", aliases)
			case "namespace":
				appendNamespaceTools(&tools, tool, aliases)
			case "custom":
				appendCustomTool(&tools, jsonutil.StringAt(tool, "name"), tool, aliases)
			}
			// tool_search, web_search, local_shell and other provider-side
			// tools have no chat representation: dropping them keeps every
			// other tool usable instead of failing the whole request.
		}
	}
	return tools, aliases
}

func appendFunctionTool(tools *[]bridgeTool, tool map[string]any, namespace string, aliases *ToolAliases) {
	name := jsonutil.StringAt(tool, "name")
	if name == "" {
		return
	}
	chatName := name
	kind := ToolAliasFunction
	if namespace != "" {
		chatName = flattenNamespaceToolName(namespace, name)
		kind = ToolAliasNamespace
	}
	*tools = append(*tools, bridgeTool{
		Name:        chatName,
		Description: jsonutil.StringAt(tool, "description"),
		Schema:      tool["parameters"],
		Strict:      jsonutil.BoolAt(tool, "strict"),
	})
	aliases.byChatName[chatName] = ToolAlias{Kind: kind, Name: name, Namespace: namespace}
}

func appendNamespaceTools(tools *[]bridgeTool, tool map[string]any, aliases *ToolAliases) {
	namespace := jsonutil.StringAt(tool, "name")
	if namespace == "" {
		return
	}
	children := jsonutil.SliceAt(tool, "tools")
	if len(children) == 0 {
		children = jsonutil.SliceAt(tool, "children")
	}
	for _, raw := range children {
		child, ok := raw.(map[string]any)
		if !ok || jsonutil.StringAt(child, "type") != "function" {
			continue
		}
		appendFunctionTool(tools, child, namespace, aliases)
	}
}

// appendCustomTool exposes a free-form custom tool as a function taking one
// string. The original definition is embedded in the description so the model
// still sees the real contract.
func appendCustomTool(tools *[]bridgeTool, name string, tool map[string]any, aliases *ToolAliases) {
	if name == "" {
		return
	}
	definition, err := json.Marshal(tool)
	if err != nil {
		definition = []byte(`{"type":"custom","name":"` + name + `"}`)
	}
	description := customToolMetadataHeading + "\n```json\n" + string(definition) + "\n```"
	*tools = append(*tools, bridgeTool{
		Name:        name,
		Description: description,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				customToolInputField: map[string]any{
					"type":        "string",
					"description": customToolInputDescription,
				},
			},
			"required": []any{customToolInputField},
		},
	})
	aliases.byChatName[name] = ToolAlias{Kind: ToolAliasCustom, Name: name}
}

// flattenNamespaceToolName renders a namespaced tool the way the chat wire
// carries it: "<namespace>__<tool>". Names that would exceed the common
// 64-character tool-name limit are truncated with a stable hash suffix.
func flattenNamespaceToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= chatToolNameMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(sum[:4])
	prefix := full[:chatToolNameMaxLen-len(suffix)]
	return prefix + suffix
}

// customToolArguments renders the chat arguments string for a custom tool call
// (the wire shape is {"input": "<raw>"}).
func customToolArguments(input string) string {
	encoded, err := json.Marshal(map[string]any{customToolInputField: input})
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// customToolInputFromArguments is the inverse: it recovers the raw string from
// a chat tool call's arguments object.
func customToolInputFromArguments(arguments string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(arguments), &payload); err != nil {
		return ""
	}
	if value, ok := payload[customToolInputField].(string); ok {
		return value
	}
	return ""
}

// applyToolAlias rewrites one chat tool call into the field set the Responses
// client declared. Unknown names stay plain function calls.
func (aliases *ToolAliases) applyToolAlias(name, arguments string) (itemType, responseName, namespace, input string) {
	itemType, responseName = "function_call", name
	alias, ok := aliases.Lookup(name)
	if !ok {
		return
	}
	switch alias.Kind {
	case ToolAliasNamespace:
		responseName, namespace = alias.Name, alias.Namespace
	case ToolAliasCustom:
		itemType, responseName = "custom_tool_call", alias.Name
		input = customToolInputFromArguments(arguments)
	}
	return
}

// aliasToolChoiceName maps a function name from a Responses tool_choice onto
// the chat name the upstream actually sees. A namespaced choice is flattened
// exactly like the tool list was.
func (aliases *ToolAliases) aliasToolChoiceName(name, namespace string) string {
	if namespace != "" {
		return flattenNamespaceToolName(namespace, name)
	}
	return name
}
