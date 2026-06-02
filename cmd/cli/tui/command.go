package tui

import (
	"fmt"
	"strings"
)

// command is a parsed console command line.
type command struct {
	verb string   // verb is the first token (faucet, transfer, object, ...)
	args []string // args are the remaining whitespace-separated tokens
}

// parseCommand splits a line into a verb and arguments, erroring on an empty line.
func parseCommand(line string) (command, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return command{}, fmt.Errorf("empty command")
	}

	return command{verb: fields[0], args: fields[1:]}, nil
}
