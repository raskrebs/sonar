package mcpserver_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/raskrebs/sonar/internal/mcpserver"
)

// TestPromptsListShowsBothPrompts pins the argument schemas a client renders:
// free_port needs a port, bring_up_project takes an optional path.
func TestPromptsListShowsBothPrompts(t *testing.T) {
	h := newHarness(t)

	res, err := h.client.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatalf("prompts/list: %v", err)
	}
	byName := map[string]*mcp.Prompt{}
	for _, p := range res.Prompts {
		byName[p.Name] = p
	}
	if len(byName) != 2 {
		t.Fatalf("prompts/list = %v, want free_port and bring_up_project", byName)
	}

	free := byName[mcpserver.PromptFreePort]
	if free == nil || len(free.Arguments) != 1 {
		t.Fatalf("free_port = %+v, want one argument", free)
	}
	if free.Arguments[0].Name != "port" || !free.Arguments[0].Required {
		t.Errorf("free_port argument = %+v, want a required port", free.Arguments[0])
	}

	bring := byName[mcpserver.PromptBringUpProject]
	if bring == nil || len(bring.Arguments) != 1 {
		t.Fatalf("bring_up_project = %+v, want one argument", bring)
	}
	if bring.Arguments[0].Name != "path" || bring.Arguments[0].Required {
		t.Errorf("bring_up_project argument = %+v, want an optional path", bring.Arguments[0])
	}
	for _, p := range res.Prompts {
		if p.Description == "" || p.Title == "" {
			t.Errorf("%s needs a title and a description: %+v", p.Name, p)
		}
	}
}

// TestFreePortPromptIsTheSpecText: the prompt is spec 2 section 1.3 verbatim,
// as one user message. The last sentence is the point of it.
func TestFreePortPromptIsTheSpecText(t *testing.T) {
	h := newHarness(t)

	text := promptText(t, h, mcpserver.PromptFreePort, map[string]string{"port": "8123"})
	want := "Inspect port 8123, explain what owns it and who started it, then propose the " +
		"least destructive way to free it. Do not kill without confirming what it is."
	if text != want {
		t.Fatalf("free_port =\n%q\nwant\n%q", text, want)
	}
}

// TestBringUpProjectDefaultsToTheWorkingDirectory: with no path the prompt
// means "this project", which for a stdio server is the directory the client
// started it in.
func TestBringUpProjectDefaultsToTheWorkingDirectory(t *testing.T) {
	h := newHarness(t)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	text := promptText(t, h, mcpserver.PromptBringUpProject, nil)
	want := "Read `sonar.yaml` in " + cwd + " via `list_groups`, start each service with " +
		"`start_service` in dependency order, wait for their ports, and report the URLs."
	if text != want {
		t.Fatalf("bring_up_project =\n%q\nwant\n%q", text, want)
	}

	text = promptText(t, h, mcpserver.PromptBringUpProject, map[string]string{"path": "/home/dev/shop"})
	if !strings.Contains(text, "in /home/dev/shop via") {
		t.Errorf("bring_up_project did not use the path it was given: %q", text)
	}
}

// TestFreePortRejectsSomethingThatIsNotAPort: a prompt has no error result the
// way a tool does, so a bad argument fails the call — with the same wording an
// invalid tool argument gets.
func TestFreePortRejectsSomethingThatIsNotAPort(t *testing.T) {
	h := newHarness(t)

	for _, args := range []map[string]string{nil, {"port": "http"}, {"port": "99999"}} {
		_, err := h.client.GetPrompt(context.Background(),
			&mcp.GetPromptParams{Name: mcpserver.PromptFreePort, Arguments: args})
		if err == nil {
			t.Fatalf("free_port %v should have failed", args)
		}
		if !strings.Contains(err.Error(), mcpserver.CodeInvalidArguments) {
			t.Errorf("free_port %v failed with %q, want %s", args, err, mcpserver.CodeInvalidArguments)
		}
	}
}

// promptText expands a prompt and returns its single user message.
func promptText(t *testing.T, h *harness, name string, args map[string]string) string {
	t.Helper()
	res, err := h.client.GetPrompt(context.Background(),
		&mcp.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("prompts/get %s: %v", name, err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("%s returned %d messages, want 1", name, len(res.Messages))
	}
	msg := res.Messages[0]
	if msg.Role != "user" {
		t.Errorf("%s role = %q, want user", name, msg.Role)
	}
	content, ok := msg.Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s content is %T, want text", name, msg.Content)
	}
	return content.Text
}
