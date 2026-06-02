package tui

import "testing"

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		verb string
		args int
		err  bool
	}{
		{"faucet 1000", "faucet", 1, false},
		{"transfer abcd ef01", "transfer", 2, false},
		{"  object   create 10 cafe ", "object", 3, false},
		{"", "", 0, true},
	}
	for _, c := range cases {
		cmd, err := parseCommand(c.in)
		if c.err {
			if err == nil {
				t.Fatalf("parseCommand(%q) expected error", c.in)
			}
			continue
		}
		if err != nil || cmd.verb != c.verb || len(cmd.args) != c.args {
			t.Fatalf("parseCommand(%q) = %+v, err %v", c.in, cmd, err)
		}
	}
}
