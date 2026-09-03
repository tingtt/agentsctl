package provider

import (
	"context"
	"io"
)

type Result struct{ Stdout, Stderr []byte }

type Runner interface {
	Run(context.Context, string, []string, string) (Result, error)
}

type Commander interface {
	Start(context.Context, string, []string, string, io.Reader, io.Writer, io.Writer) error
}
