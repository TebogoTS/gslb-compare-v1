// gslb-compare inventories k8gb Gslb objects and their Ingresses on RKE1 downstream
// clusters (one Rancher), and checks which of those hosts already exist on RKE2 downstream
// clusters spread across several Rancher management servers.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/alecthomas/kong"
)

var cli struct {
	Config string `help:"Config file." default:"config.json" short:"c"`

	Clusters clustersCmd `cmd:"" help:"List clusters on every endpoint and show how names are parsed (no downstream calls)."`
	Collect  collectCmd  `cmd:"" help:"Walk every downstream cluster and save Gslb/Ingress data."`
	Report   reportCmd   `cmd:"" help:"Build the RKE1 inventory / found-in-RKE2 / missing-in-RKE2 lists offline from a snapshot."`
	Run      runCmd      `cmd:"" help:"Collect + report; snapshot saved to <out>/snapshot.json."`
}

type clustersCmd struct{}

type collectCmd struct {
	Snapshot string `help:"Snapshot output file." default:"snapshot.json"`
	Workers  int    `help:"Override config workers (0 = use config)."`
}

type reportCmd struct {
	Snapshot string `help:"Snapshot input file." default:"snapshot.json"`
	Out      string `help:"Report output directory." default:"report"`
	Env      string `help:"Env to scope both sides to (both DCs); empty means no filter." default:"nonprod"`
}

type runCmd struct {
	Out     string `help:"Report output directory." default:"report"`
	Env     string `help:"Env to scope both sides to (both DCs); empty means no filter." default:"nonprod"`
	Workers int    `help:"Override config workers (0 = use config)."`
}

func (c *clustersCmd) Run(cfg *Config, ctx context.Context) error {
	return cmdClusters(ctx, cfg)
}

func (c *collectCmd) Run(cfg *Config, ctx context.Context) error {
	if c.Workers > 0 {
		cfg.Workers = c.Workers
	}
	snap, err := runCollect(ctx, cfg)
	if err != nil {
		return err
	}
	return saveSnapshot(c.Snapshot, snap)
}

func (c *reportCmd) Run(cfg *Config, ctx context.Context) error {
	snap, err := loadSnapshot(c.Snapshot)
	if err != nil {
		return err
	}
	return doReport(cfg, snap, c.Out, c.Env)
}

func (c *runCmd) Run(cfg *Config, ctx context.Context) error {
	if c.Workers > 0 {
		cfg.Workers = c.Workers
	}
	snap, err := runCollect(ctx, cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.Out, 0o755); err != nil {
		return err
	}
	if err := saveSnapshot(filepath.Join(c.Out, "snapshot.json"), snap); err != nil {
		return err
	}
	return doReport(cfg, snap, c.Out, c.Env)
}

func main() {
	kctx := kong.Parse(&cli,
		kong.Name("gslb-compare"),
		kong.Description("RKE1 vs RKE2 k8gb Gslb/Ingress inventory and comparison.\n\n"+
			"Tokens are read from the env vars named by each endpoint's tokenEnv.\n"+
			"HTTPS_PROXY / NO_PROXY are honoured. -env scopes both sides to that env's\n"+
			"clusters (both DCs); the default, \"nonprod\", covers nonprod/270 + nonprod/sdc."),
		kong.UsageOnError(),
	)

	cfg, err := loadConfig(cli.Config)
	kctx.FatalIfErrorf(err)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = kctx.Run(cfg, ctx)
	kctx.FatalIfErrorf(err)
}

func doReport(cfg *Config, snap *Snapshot, out, env string) error {
	rep := buildSimpleReport(cfg, snap, env)
	if err := writeSimpleReports(rep, out); err != nil {
		return err
	}
	printSimpleSummary(os.Stdout, rep)
	fmt.Printf("\nreport written to %s/\n", out)
	return nil
}

func cmdClusters(ctx context.Context, cfg *Config) error {
	snap, _, err := discover(ctx, cfg)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ENDPOINT\tROLE\tCLUSTER\tID\tSTATE\tGROUP\tSLOT\tLEGACY-HINT")
	for _, c := range snap.Clusters {
		group, slot := "", ""
		if c.Role == "legacy" {
			var ok bool
			if group, slot, ok = cfg.parseLegacy(c.Name); !ok {
				group = "!! no match"
			}
		} else {
			slot = cfg.targetSlot(c.Endpoint, c.Name)
			if slot == "" {
				slot = "!! no match"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Endpoint, c.Role, c.Name, c.ID,
			firstNonEmpty(c.State, "?"), group, slot, strings.TrimSpace(c.LegacyHint))
	}
	return tw.Flush()
}

func saveSnapshot(path string, s *Snapshot) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("snapshot written to %s (%d clusters)\n", path, len(s.Clusters))
	return nil
}

func loadSnapshot(path string) (*Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	return &s, json.Unmarshal(b, &s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
