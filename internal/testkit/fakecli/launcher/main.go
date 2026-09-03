package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func main() {
	python, err := exec.LookPath("python3")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(127)
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(127)
	}
	args := append([]string{"python3", executable + ".py"}, os.Args[1:]...)
	if err := syscall.Exec(python, args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(126)
	}
}
