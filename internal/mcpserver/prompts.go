package mcpserver

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The prompts (spec 2, section 1.3). Both are thin on purpose: they are the
// two moments where a person reaches for sonar and would otherwise have to
// write the instruction themselves, and the text is the workflow the tools
// were designed around — find out who owns a port before freeing it, and bring
// a project up in dependency order rather than one server at a time.

// PromptFreePort and PromptBringUpProject are the prompt names.
const (
	PromptFreePort        = "free_port"
	PromptBringUpProject  = "bring_up_project"
	freePortDescription   = "Work out what is holding a port and how to free it without breaking someone else's work."
	bringUpProjectDescrip = "Start every service a project declares, in dependency order, and report the URLs."
)

func init() { registerPrompts((*Server).addPrompts) }

func (s *Server) addPrompts() {
	s.mcp.AddPrompt(&mcp.Prompt{
		Name:        PromptFreePort,
		Title:       "Free a port",
		Description: freePortDescription,
		Arguments: []*mcp.PromptArgument{{
			Name:        "port",
			Title:       "Port",
			Description: "The TCP port to free, for example 3000.",
			Required:    true,
		}},
	}, freePortPrompt)

	s.mcp.AddPrompt(&mcp.Prompt{
		Name:        PromptBringUpProject,
		Title:       "Bring up a project",
		Description: bringUpProjectDescrip,
		Arguments: []*mcp.PromptArgument{{
			Name:        "path",
			Title:       "Project path",
			Description: "The project directory holding the sonar.yaml. Defaults to the working directory the MCP server was started in.",
		}},
	}, bringUpProjectPrompt)
}

// freePortPrompt is spec 2's text verbatim. The last sentence is the whole
// point of the prompt: the model is being asked to find the owner, not to kill
// the process.
func freePortPrompt(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	port, err := portArgument(req.Params.Arguments["port"])
	if err != nil {
		return nil, err
	}
	return userPrompt(freePortDescription, fmt.Sprintf(
		"Inspect port %d, explain what owns it and who started it, then propose the least "+
			"destructive way to free it. Do not kill without confirming what it is.", port)), nil
}

// bringUpProjectPrompt defaults the path to the server's working directory,
// which for a stdio server is the directory the client (and so the developer)
// is working in.
func bringUpProjectPrompt(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	path := strings.TrimSpace(req.Params.Arguments["path"])
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, promptError(fmt.Sprintf(
				"%s needs a path: this server has no working directory of its own (%v)",
				PromptBringUpProject, err))
		}
		path = cwd
	}
	return userPrompt(bringUpProjectDescrip, fmt.Sprintf(
		"Read `sonar.yaml` in %s via `list_groups`, start each service with `start_service` "+
			"in dependency order, wait for their ports, and report the URLs.", path)), nil
}

// portArgument parses the one argument free_port takes. Prompt arguments are
// strings on the wire, so "3000" and " 3000 " are the same port and "http" is
// an error the client can show rather than a prompt that reads as nonsense.
func portArgument(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, promptError(PromptFreePort + " needs a port, for example {\"port\": \"3000\"}")
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, promptError(fmt.Sprintf("%q is not a TCP port; pass a number between 1 and 65535", raw))
	}
	return port, nil
}

// userPrompt wraps text as the single user message a prompt expands to. Both
// prompts are instructions to the model, not context for it, so they are user
// messages rather than assistant ones.
func userPrompt(description, text string) *mcp.GetPromptResult {
	return &mcp.GetPromptResult{
		Description: description,
		Messages: []*mcp.PromptMessage{{
			Role:    "user",
			Content: &mcp.TextContent{Text: text},
		}},
	}
}

// promptError is an argument failure. A prompt has no IsError channel the way
// a tool result does, so it fails the call with the invalid-params code and
// the tools' "<code>: <message>" wording.
func promptError(message string) error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInvalidParams,
		Message: CodeInvalidArguments + ": " + message,
	}
}
