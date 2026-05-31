package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"BluePods/client"
)

// watchInterval is how often the watch dashboard reprints.
const watchInterval = time.Second

// cmdWatch reprints a compact dashboard every interval until interrupted.
func cmdWatch(e *env, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	objectsArg := fs.String("objects", "", "comma-separated object IDs (hex) to track")

	if err := fs.Parse(args); err != nil {
		return err
	}

	objectIDs, err := parseObjectList(*objectsArg)
	if err != nil {
		return err
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	renderDashboard(e, cli, objectIDs)

	for {
		select {
		case <-stop:
			fmt.Println("\nstopped")
			return nil
		case <-ticker.C:
			renderDashboard(e, cli, objectIDs)
		}
	}
}

// parseObjectList splits a comma-separated hex list into object IDs.
func parseObjectList(arg string) ([][32]byte, error) {
	if arg == "" {
		return nil, nil
	}

	parts := strings.Split(arg, ",")
	ids := make([][32]byte, 0, len(parts))

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		id, err := parseHash(p)
		if err != nil {
			return nil, fmt.Errorf("parse object id %q:\n%w", p, err)
		}

		ids = append(ids, id)
	}

	return ids, nil
}

// renderDashboard clears the screen and prints node status and tracked objects.
func renderDashboard(e *env, cli *client.Client, objectIDs [][32]byte) {
	fmt.Print("\033[H\033[2J") // clear screen, cursor home
	fmt.Printf("bpctl watch  %s  node=%s\n\n", time.Now().Format("15:04:05"), e.nodeAddr)

	renderNodeLine(cli)
	renderObjects(cli, objectIDs)
}

// renderNodeLine prints the node's round, epoch, and validator count.
func renderNodeLine(cli *client.Client) {
	status, err := cli.Status()
	if err != nil {
		fmt.Printf("node: unreachable (%v)\n", err)
		return
	}

	fmt.Printf("round=%d  epoch=%d  validators=%d  lastCommit=%d\n",
		status.Round, status.Epoch, status.Validators, status.LastCommitted)
}

// renderObjects prints version, owner, and holder count for each tracked object.
func renderObjects(cli *client.Client, objectIDs [][32]byte) {
	if len(objectIDs) == 0 {
		return
	}

	vals, valErr := cli.Validators()

	fmt.Println("\nobjects:")

	for _, id := range objectIDs {
		obj, err := cli.GetObject(id)
		if err != nil {
			fmt.Printf("  %s  <unavailable>\n", hex.EncodeToString(id[:8]))
			continue
		}

		holders := "?"
		if valErr == nil {
			holders = fmt.Sprintf("%d", len(probeHolders(vals, id)))
		}

		fmt.Printf("  %s  v%d  owner=%s  holders=%s\n",
			hex.EncodeToString(id[:8]), obj.Version, hex.EncodeToString(obj.Owner[:8]), holders)
	}
}
