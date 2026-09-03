package provider

import (
	"bytes"
	"context"
	"io"
	"os/exec"
)

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, path string, args []string, cwd string) (Result, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = cwd
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	return Result{Stdout: out.Bytes(), Stderr: errOut.Bytes()}, err
}

func (ExecRunner) Start(ctx context.Context, path string, args []string, cwd string, in io.Reader, out, errOut io.Writer) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = cwd
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errOut
	return cmd.Run()
}
