package manifesteval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Subcommand is the hidden argv[1] serve-multi dispatches to ServeStdio.
const Subcommand = "manifest-eval"

// subprocessTimeout bounds the whole child run — spawn, rlimit, eval, write.
// Comfortably above Timeout (the child's own interrupt) so the interrupt is
// what normally fires; the process kill is the backstop for a child wedged
// outside JS execution.
const subprocessTimeout = 15 * time.Second

// request is the stdin wire shape parent → child.
type request struct {
	Name string `json:"name"`
	Src  string `json:"src"`
}

// EvalSubprocess runs one manifest eval in a child process built from argv
// (typically {selfExe, Subcommand}) and returns the marshalled manifest
// JSON. The child limits its own address space before reading the input, so
// an allocation bomb kills the child — never the router.
func EvalSubprocess(ctx context.Context, argv []string, src, name string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("manifesteval: empty argv")
	}
	ctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()

	input, err := json.Marshal(request{Name: name, Src: src})
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			// A child killed without a word (rlimit-tripped runtime OOM dies
			// with a fatal error, but SIGKILL from the context leaves nothing)
			// still deserves a diagnosis the operator can act on.
			msg = fmt.Sprintf("evaluator process died (%v) — a runaway manifest (memory bomb?) is the usual cause", err)
		}
		return nil, fmt.Errorf("evaluate %s: %s", name, firstLines(msg, 5))
	}
	return stdout.Bytes(), nil
}

// ServeStdio is the child side: apply the memory limit, read one request from
// stdin, evaluate, write the manifest JSON to stdout. Errors go to stderr
// with a non-zero exit. Called by serve-multi before any other init — the
// child must not boot a control plane just to run 5ms of JS.
func ServeStdio() int {
	applyMemoryLimit()

	body, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "read request: %v\n", err)
		return 1
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		fmt.Fprintf(os.Stderr, "decode request: %v\n", err)
		return 1
	}

	data, err := EvalJSON(req.Src, req.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if _, err := os.Stdout.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "write result: %v\n", err)
		return 1
	}
	return 0
}

// firstLines truncates a child's stderr for the returned error.
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + " …"
	}
	return s
}
