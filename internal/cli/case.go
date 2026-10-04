package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func openCase(path string) (*evidence.Case, error) {
	if path == "" {
		return nil, usageErrorf("--case is required")
	}
	return evidence.Open(path)
}

func newCaseCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := newGroupCmd("case", "Create, inspect, upgrade and verify cases")
	cmd.AddCommand(newCaseNewCmd(d, opts), newCaseInfoCmd(d, opts), newCaseVerifyCmd(d, opts), newCaseUpgradeCmd(d, opts))
	return cmd
}

func newCaseUpgradeCmd(d Deps, opts *rootOptions) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the case database to the current schema (audited; never implicit)",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := openCase(path)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			res, err := c.Upgrade()
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(d.Out, map[string]any{"from": res.From, "to": res.To, "upgraded": res.Upgraded, "resumed": res.Resumed, "records_to_index": res.RecordsToIndex})
			}
			switch {
			case res.Resumed:
				fmt.Fprintf(d.Out, "case schema v%d: concluded an interrupted upgrade (audited)\n", res.To)
			case res.Upgraded:
				fmt.Fprintf(d.Out, "upgraded case schema v%d -> v%d\n", res.From, res.To)
			default:
				fmt.Fprintf(d.Out, "case schema v%d is already current\n", res.To)
			}
			if res.RecordsToIndex > 0 {
				fmt.Fprintf(d.Out, "%d records are not searchable until the full-text index is built; run: minutiae records reindex --case %s\n", res.RecordsToIndex, c.Dir)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "case", "", "case directory")
	_ = cmd.MarkFlagRequired("case")
	return cmd
}

func newCaseNewCmd(d Deps, opts *rootOptions) *cobra.Command {
	var dir string
	var co evidence.CreateOptions
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new case directory",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := evidence.Create(dir, co)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			if opts.json {
				return writeJSON(d.Out, map[string]any{"dir": c.Dir, "meta": c.Meta})
			}
			fmt.Fprintf(d.Out, "created case %s at %s\n", c.Meta.ID, c.Dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "parent directory for the case")
	cmd.Flags().StringVar(&co.ID, "id", "", "case identifier")
	cmd.Flags().StringVar(&co.Examiner, "examiner", "", "examiner name")
	cmd.Flags().StringVar(&co.Description, "description", "", "case description")
	for _, f := range []string{"dir", "id", "examiner"} {
		_ = cmd.MarkFlagRequired(f)
	}
	return cmd
}

func newCaseInfoCmd(d Deps, opts *rootOptions) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Show case metadata and artifact count",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := openCase(path)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			recs, err := c.Manifest()
			if err != nil {
				return err
			}
			schema, err := c.SchemaVersion()
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(d.Out, map[string]any{
					"id": c.Meta.ID, "examiner": c.Meta.Examiner, "description": c.Meta.Description,
					"created": c.Meta.Created, "tool_version": c.Meta.ToolVersion, "artifacts": len(recs),
					"schema_version": schema,
				})
			}
			fmt.Fprintf(d.Out, "Case:        %s\nExaminer:    %s\nCreated:     %s\nTool:        %s\nArtifacts:   %d\nSchema:      v%d",
				c.Meta.ID, c.Meta.Examiner, c.Meta.Created, c.Meta.ToolVersion, len(recs), schema)
			if schema < evidence.CurrentSchema {
				fmt.Fprintf(d.Out, " (older than v%d; run: minutiae case upgrade --case %s)", evidence.CurrentSchema, c.Dir)
			}
			fmt.Fprintln(d.Out)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "case", "", "case directory")
	_ = cmd.MarkFlagRequired("case")
	return cmd
}

func newCaseVerifyCmd(d Deps, opts *rootOptions) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Re-hash all artifacts and check the audit chain",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c, err := openCase(path)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			rep, err := c.Verify()
			if err != nil {
				return err
			}
			if opts.json {
				if err := writeJSON(d.Out, rep); err != nil {
					return err
				}
			} else {
				for _, n := range rep.Notices {
					fmt.Fprintln(d.Out, "NOTICE:", escapeText(n))
				}
				for _, p := range rep.Problems {
					fmt.Fprintln(d.Out, "PROBLEM:", escapeText(p))
				}
				status := "OK"
				if !rep.OK() {
					status = "FAILED"
				}
				fmt.Fprintf(d.Out, "%s: %d artifacts%s, %d audit entries, %d problems\n",
					status, rep.ArtifactsChecked, rep.RecordsSummary(), rep.AuditEntries, len(rep.Problems))
			}
			if !rep.OK() {
				return fmt.Errorf("%w: %d problem(s)", evidence.ErrIntegrity, len(rep.Problems))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "case", "", "case directory")
	_ = cmd.MarkFlagRequired("case")
	return cmd
}
