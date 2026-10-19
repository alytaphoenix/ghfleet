package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func actionsCmd() *cobra.Command {
	var limit int
	root := &cobra.Command{
		Use:   "actions [repo]",
		Short: "Inspect and control GitHub Actions runs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runActionsList(cmd, args, limit)
		},
	}
	root.Flags().IntVar(&limit, "limit", 20, "maximum runs to show")
	root.AddCommand(actionsListCmd(), actionsJobsCmd(), actionsRerunCmd(), actionsCancelCmd(), actionsDispatchCmd(), actionsWorkflowsCmd())
	return root
}

func actionsListCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:     "list [repo]",
		Aliases: []string{"ls"},
		Short:   "List recent workflow runs",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runActionsList(cmd, args, limit)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum runs to show")
	return cmd
}

func runActionsList(cmd *cobra.Command, args []string, limit int) error {
	repo := ""
	if len(args) == 1 {
		repo = args[0]
	}
	runs, err := newClient().Actions(context.Background(), repo, limit)
	if err != nil {
		return err
	}
	if flagJSON {
		return printJSON(runs)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "REPO\tRUN\tWORKFLOW\tSTATUS\tCONCLUSION\tBRANCH\tAGE\tTITLE")
	for _, run := range runs {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			run.Repo, run.ID, truncate(empty(run.WorkflowName, run.Name), 28),
			run.Status, empty(run.Conclusion, "-"), empty(run.HeadBranch, "-"),
			ago(run.CreatedAt), truncate(run.DisplayTitle, 55))
	}
	return w.Flush()
}

func actionsJobsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "jobs <repo> <run-id>",
		Short: "Show jobs and steps for one workflow run",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := parseRunID(args[1])
			if err != nil {
				return err
			}
			jobs, err := newClient().ActionJobs(context.Background(), args[0], runID)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(jobs)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "JOB\tSTATUS\tCONCLUSION\tSTARTED\tDURATION\tNAME")
			for _, job := range jobs {
				dur := time.Duration(0)
				if !job.StartedAt.IsZero() && !job.CompletedAt.IsZero() {
					dur = job.CompletedAt.Sub(job.StartedAt)
				}
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", job.ID, job.Status,
					empty(job.Conclusion, "-"), ago(job.StartedAt), empty(dur.Round(time.Second).String(), "-"), job.Name)
			}
			return w.Flush()
		},
	}
}

func actionsRerunCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "rerun <repo> <run-id>",
		Short: "Rerun failed jobs (or all jobs with --all)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := parseRunID(args[1])
			if err != nil {
				return err
			}
			if err := newClient().RerunAction(context.Background(), args[0], runID, !all); err != nil {
				return err
			}
			fmt.Printf("rerun requested for %s run %d\n", args[0], runID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "rerun all jobs instead of only failed jobs")
	return cmd
}

func actionsCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <repo> <run-id>",
		Short: "Cancel a queued or running workflow run",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := parseRunID(args[1])
			if err != nil {
				return err
			}
			if err := newClient().CancelAction(context.Background(), args[0], runID); err != nil {
				return err
			}
			fmt.Printf("cancel requested for %s run %d\n", args[0], runID)
			return nil
		},
	}
}

func actionsDispatchCmd() *cobra.Command {
	var ref string
	var inputs []string
	cmd := &cobra.Command{
		Use:   "dispatch <repo> <workflow>",
		Short: "Trigger a workflow_dispatch workflow",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsed := map[string]string{}
			for _, raw := range inputs {
				k, v, ok := strings.Cut(raw, "=")
				if !ok || k == "" {
					return fmt.Errorf("input %q must be key=value", raw)
				}
				parsed[k] = v
			}
			if err := newClient().DispatchAction(context.Background(), args[0], args[1], ref, parsed); err != nil {
				return err
			}
			fmt.Printf("workflow %s dispatched on %s@%s\n", args[1], args[0], ref)
			return nil
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "main", "branch or tag to run the workflow on")
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "workflow input key=value (repeatable)")
	return cmd
}

func actionsWorkflowsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "workflows <repo>",
		Short: "List workflow definitions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wfs, err := newClient().ActionWorkflows(context.Background(), args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(wfs)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tSTATE\tPATH\tNAME")
			for _, wf := range wfs {
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", wf.ID, wf.State, wf.Path, wf.Name)
			}
			return w.Flush()
		},
	}
}

func reviewPRCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "review-pr <repo> <number>",
		Short: "Enqueue a read-only agent review for a tracked PR",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil || n <= 0 {
				return fmt.Errorf("invalid PR number %q", args[1])
			}
			job, err := newClient().ReviewPR(context.Background(), args[0], n)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(job)
			}
			fmt.Printf("review enqueued: %s (%s)\n", job.ID, job.Status)
			return nil
		},
	}
}

func parseRunID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid run id %q", raw)
	}
	return id, nil
}
