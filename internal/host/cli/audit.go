package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func (a *app) auditCmd() *cobra.Command {
	var follow, all bool
	var lines int
	var session string
	c := &cobra.Command{
		Use:   "audit",
		Short: "Show the guest's activity log (executions, connections, writes)",
		Long: `Show the guest's activity log. The guest writes it to a serial port that
the host appends to a file per boot (~/.local/state/hanare/guests/<ID>/audit/),
so it survives the guest and cannot be rewritten from inside it.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			g, err := a.pick(c.Context(), nil)
			if err != nil {
				return err
			}
			files, err := g.AuditFiles()
			if err != nil {
				return err
			}
			if len(files) == 0 {
				return fmt.Errorf("no audit log for guest %s yet", g.Name())
			}
			if !all {
				files = files[len(files)-1:]
			}
			var recs []string
			for _, f := range files {
				b, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				for _, l := range strings.Split(string(b), "\n") {
					if l = strings.TrimSpace(l); l != "" && match(l, session) {
						recs = append(recs, l)
					}
				}
			}
			if lines > 0 && len(recs) > lines {
				recs = recs[len(recs)-lines:]
			}
			for _, l := range recs {
				a.printAudit(os.Stdout, l)
			}
			if follow {
				return a.followAudit(files[len(files)-1], session)
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new records")
	c.Flags().IntVarP(&lines, "lines", "n", 50, "number of records (0: all)")
	c.Flags().BoolVar(&all, "all", false, "include earlier boots")
	c.Flags().StringVar(&session, "session", "", "only records of this session")
	a.jsonFlag(c)
	return c
}

func match(line, session string) bool {
	if session == "" {
		return true
	}
	var r struct{ Session string }
	return json.Unmarshal([]byte(line), &r) == nil && r.Session == session
}

// printAudit prints a record as JSON (--json) or as one readable line.
func (a *app) printAudit(w io.Writer, line string) {
	if a.json {
		fmt.Fprintln(w, line)
		return
	}
	var r struct {
		Time, Session, Event, Comm, Path, Dest string
		PID                                    uint32
		Argv                                   []string
		Count                                  uint64
	}
	if json.Unmarshal([]byte(line), &r) != nil {
		fmt.Fprintf(w, "(unparsable) %q\n", line)
		return
	}
	t, _ := time.Parse(time.RFC3339Nano, r.Time)
	what := r.Path
	switch r.Event {
	case "exec":
		what = strings.Join(r.Argv, " ")
		if what == "" {
			what = r.Path
		}
	case "connect", "send":
		what = r.Dest
	case "lost":
		what = fmt.Sprintf("%d events lost", r.Count)
	}
	fmt.Fprintf(w, "%s  %-10s %-7d %-16s %s  [%s]\n", t.Local().Format("15:04:05"), r.Event, r.PID, r.Comm, what, r.Session)
}

func (a *app) followAudit(file, session string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	f.Seek(0, io.SeekEnd)
	r := bufio.NewReader(f)
	for {
		l, err := r.ReadString('\n')
		if err == io.EOF {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
		if l = strings.TrimSpace(l); l != "" && match(l, session) {
			a.printAudit(os.Stdout, l)
		}
	}
}
