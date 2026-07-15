package harness

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// stopTimeout bounds how long Stop waits for a graceful exit before
// escalating to Kill.
const stopTimeout = 15 * time.Second

// NodeArgs configures a node process launch. Bootstrap and BootstrapAddr are
// mutually exclusive: Bootstrap starts a fresh genesis validator with no
// upstream; a non-empty BootstrapAddr starts a validator that syncs from and
// registers against that address. The remaining fields are cluster-wide
// tuning, unchanged across a node's restarts.
type NodeArgs struct {
	Bootstrap        bool   // Bootstrap starts this node as the genesis validator
	BootstrapAddr    string // BootstrapAddr is the alive node this validator syncs from and registers against
	SystemPod        string // SystemPod is the path to the system pod WASM
	MinValidators    int    // MinValidators is the consensus threshold (0 = node default)
	SyncBuffer       int    // SyncBuffer is the sync buffer in seconds (0 = node default)
	EpochLength      uint64 // EpochLength is rounds per epoch (0 = node default)
	MaxChurn         int    // MaxChurn is the max validator changes per epoch (0 = unlimited)
	GossipFanout     int    // GossipFanout is peers per vertex gossip (0 = node default)
	TransitionGrace  int    // TransitionGrace is grace rounds after minValidators is reached (0 = node default)
	TransitionBuffer int    // TransitionBuffer is buffer rounds after the grace period (0 = node default)
	InitialMint      uint64 // InitialMint is the bootstrap mint amount (0 = node default)
}

// Node is one managed node process: its identity (data directory, key, QUIC
// address) is fixed at construction and survives Stop/Kill/Restart, so a
// restarted node resumes the same identity over the same data.
type Node struct {
	Index    int    // Index is the node's position in the cluster
	Dir      string // Dir is the node's data directory
	KeyPath  string // KeyPath is the Ed25519 key file
	QUICAddr string // QUICAddr is the node's listen address

	binaryPath string // binaryPath is the compiled node binary shared by every node

	mu        sync.Mutex    // mu guards every field below
	args      NodeArgs      // args is the last NodeArgs passed to Start, reused (role-adjusted) by Restart
	bootstrap bool          // bootstrap records whether THIS node is the cluster's bootstrap identity
	cmd       *exec.Cmd     // cmd is the current live process, nil before the first Start
	alive     bool          // alive is true between a successful Start and the process exiting
	exitErr   error         // exitErr is the last process exit error, if any
	done      chan struct{} // done closes when the current process (and its stdout pump) has fully exited
	stdoutLog *os.File      // stdoutLog is the open <Dir>/stdout.log handle
	stderrLog *os.File      // stderrLog is the open <Dir>/stderr.log handle

	journal *Journal // journal is this node's event journal, stable across restarts
}

// newNode creates a Node with a fixed identity under dir. It does not start a
// process; call Start to do that.
func newNode(index int, dir, binaryPath, quicAddr string) (*Node, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create node dir %s:\n%w", dir, err)
	}

	return &Node{
		Index:      index,
		Dir:        dir,
		KeyPath:    filepath.Join(dir, "key"),
		QUICAddr:   quicAddr,
		binaryPath: binaryPath,
		journal:    NewJournal(fmt.Sprintf("node-%d", index)),
	}, nil
}

// Start spawns the node process with args and wires the stdout pump into the
// journal. It stores args (role-adjusted by future Restart calls) and
// records whether this is the cluster's bootstrap identity.
func (n *Node) Start(args NodeArgs) error {
	n.mu.Lock()
	n.args = args
	n.bootstrap = args.Bootstrap
	prevStdout, prevStderr := n.stdoutLog, n.stderrLog
	n.mu.Unlock()

	// A prior generation's log handles (from an earlier Start/Restart) are no
	// longer needed once its process and pump have fully quiesced, which is
	// guaranteed by the time Start is called again (Restart's caller always
	// stops or kills the previous generation first).
	if prevStdout != nil {
		prevStdout.Close()
	}
	if prevStderr != nil {
		prevStderr.Close()
	}

	stdoutLog, err := os.OpenFile(filepath.Join(n.Dir, "stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open stdout.log for node %d:\n%w", n.Index, err)
	}

	stderrLog, err := os.OpenFile(filepath.Join(n.Dir, "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		stdoutLog.Close()
		return fmt.Errorf("open stderr.log for node %d:\n%w", n.Index, err)
	}

	cmd := exec.Command(n.binaryPath, n.buildArgs(args)...)
	cmd.Stderr = stderrLog

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdoutLog.Close()
		stderrLog.Close()
		return fmt.Errorf("stdout pipe for node %d:\n%w", n.Index, err)
	}

	if err := cmd.Start(); err != nil {
		stdoutLog.Close()
		stderrLog.Close()
		return fmt.Errorf("start node %d:\n%w", n.Index, err)
	}

	done := make(chan struct{})

	n.mu.Lock()
	n.cmd = cmd
	n.alive = true
	n.exitErr = nil
	n.done = done
	n.stdoutLog = stdoutLog
	n.stderrLog = stderrLog
	n.mu.Unlock()

	var pumpDone sync.WaitGroup
	pumpDone.Add(1)

	go n.pumpStdout(stdout, &pumpDone)
	go n.awaitExit(cmd, done, &pumpDone)

	return nil
}

// buildArgs constructs the node's command-line arguments. The harness always
// runs nodes with --log-format=json and --test-hooks: the journal requires
// the former, and the invariant checker and partition scenarios require the
// latter.
func (n *Node) buildArgs(args NodeArgs) []string {
	out := []string{
		"--data", n.Dir,
		"--quic", n.QUICAddr,
		"--key", n.KeyPath,
		"--system-pod", args.SystemPod,
		"--log-format", "json",
		"--test-hooks",
	}

	if args.MinValidators > 0 {
		out = append(out, "--min-validators", strconv.Itoa(args.MinValidators))
	}
	if args.SyncBuffer > 0 {
		out = append(out, "--sync-buffer", strconv.Itoa(args.SyncBuffer))
	}
	if args.EpochLength > 0 {
		out = append(out, "--epoch-length", strconv.FormatUint(args.EpochLength, 10))
	}
	if args.MaxChurn > 0 {
		out = append(out, "--max-churn", strconv.Itoa(args.MaxChurn))
	}
	if args.GossipFanout > 0 {
		out = append(out, "--gossip-fanout", strconv.Itoa(args.GossipFanout))
	}
	if args.TransitionGrace > 0 {
		out = append(out, "--transition-grace", strconv.Itoa(args.TransitionGrace))
	}
	if args.TransitionBuffer > 0 {
		out = append(out, "--transition-buffer", strconv.Itoa(args.TransitionBuffer))
	}
	if args.InitialMint > 0 {
		out = append(out, "--initial-mint", strconv.FormatUint(args.InitialMint, 10))
	}

	if args.Bootstrap {
		out = append(out, "--bootstrap")
	} else {
		out = append(out, "--bootstrap-addr", args.BootstrapAddr)
	}

	return out
}

// pumpStdout reads complete lines from the process's stdout, appending every
// raw line (terminated or not) to stdout.log and feeding only newline-
// terminated lines to the journal. A final unterminated chunk after a hard
// kill is dropped from the journal by construction: it is never followed by
// a newline before the pipe closes. A journal parse error is recorded on the
// node, not treated as fatal at pump time.
func (n *Node) pumpStdout(r io.Reader, done *sync.WaitGroup) {
	defer done.Done()

	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadString('\n')

		if len(line) > 0 {
			n.mu.Lock()
			log := n.stdoutLog
			n.mu.Unlock()
			if log != nil {
				log.WriteString(line)
			}

			if strings.HasSuffix(line, "\n") {
				n.journal.Append([]byte(line)) //nolint:errcheck // stored on the node below
			}
		}

		if err != nil {
			return
		}
	}
}

// awaitExit waits for the process to exit and for its stdout pump to drain,
// then closes done so Stop/Kill/WaitEvent can observe the generation as
// fully finished.
func (n *Node) awaitExit(cmd *exec.Cmd, done chan struct{}, pump *sync.WaitGroup) {
	err := cmd.Wait()
	pump.Wait()

	n.mu.Lock()
	n.alive = false
	n.exitErr = err
	n.mu.Unlock()

	close(done)
}

// Stop sends SIGTERM and waits for the process to exit, escalating to Kill
// if it does not exit within stopTimeout.
func (n *Node) Stop() error {
	n.mu.Lock()
	cmd := n.cmd
	done := n.done
	n.mu.Unlock()

	if cmd == nil || cmd.Process == nil || !n.Alive() {
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal node %d:\n%w", n.Index, err)
	}

	select {
	case <-done:
		return nil
	case <-time.After(stopTimeout):
		n.Kill()
		return nil
	}
}

// Kill sends SIGKILL and waits for the process (and its stdout pump) to
// fully exit.
func (n *Node) Kill() {
	n.mu.Lock()
	cmd := n.cmd
	done := n.done
	n.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	cmd.Process.Kill()

	if done != nil {
		<-done
	}
}

// Restart starts the same node again: same key, data directory and port,
// opening a new journal segment first. The bootstrap node restarts with
// --bootstrap and NO --bootstrap-addr; every other node restarts with
// --bootstrap-addr=syncFrom (an alive node, not necessarily its original
// bootstrap) and no --bootstrap.
func (n *Node) Restart(syncFrom string) error {
	n.mu.Lock()
	args := n.args
	bootstrap := n.bootstrap
	n.mu.Unlock()

	if bootstrap {
		args.Bootstrap = true
		args.BootstrapAddr = ""
	} else {
		args.Bootstrap = false
		args.BootstrapAddr = syncFrom
	}

	n.journal.NewSegment()

	return n.Start(args)
}

// Alive reports whether the process is currently running.
func (n *Node) Alive() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.alive
}

// Journal returns this node's event journal.
func (n *Node) Journal() *Journal {
	return n.journal
}

// ParseError returns the first schema-drift error seen on this node's stdout
// stream, if any.
func (n *Node) ParseError() error {
	return n.journal.ParseError()
}

// WaitEvent blocks until an event matching name and preds is recorded, ctx
// ends, or the process exits before the event ever arrives (in which case
// the error reports the exit cause instead of a plain timeout).
func (n *Node) WaitEvent(ctx context.Context, name string, preds ...Pred) (Event, error) {
	n.mu.Lock()
	done := n.done
	n.mu.Unlock()

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stop := make(chan struct{})
	defer close(stop)

	if done != nil {
		go func() {
			select {
			case <-done:
				cancel()
			case <-stop:
			}
		}()
	}

	e, err := n.journal.Wait(waitCtx, name, preds...)
	if err != nil && ctx.Err() == nil && !n.Alive() {
		n.mu.Lock()
		exitErr := n.exitErr
		n.mu.Unlock()

		return Event{}, fmt.Errorf("node %d exited before event %q (exit: %v), see %s",
			n.Index, name, exitErr, filepath.Join(n.Dir, "stderr.log"))
	}

	return e, err
}
