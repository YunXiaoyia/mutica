package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// Pipeline commands (WP-2, docs/aris-paper-pipeline.md): the orchestrator
// agent's entry points for starting and advancing a paper production line.
// Running `pipeline instantiate` from inside a chat task auto-binds the run
// to that chat session, so progress reports land back in the conversation.

var pipelineCmd = &cobra.Command{
	Use:   "pipeline",
	Short: "Work with pipeline templates and runs",
}

var pipelineTemplatesCmd = &cobra.Command{
	Use:   "templates",
	Short: "List pipeline templates with their stage plans",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := newAPIClient(cmd)
		if err != nil {
			return err
		}
		ctx, cancel := cli.APIContext(cmd.Context())
		defer cancel()
		var out json.RawMessage
		if err := client.GetJSON(ctx, "/api/pipeline-templates", &out); err != nil {
			return fmt.Errorf("list pipeline templates: %w", err)
		}
		return printPipelineJSON(out)
	},
}

var pipelineInstantiateCmd = &cobra.Command{
	Use:   "instantiate --template <id> [--title <t>] [--description <d>]",
	Short: "Start a pipeline run from a template",
	Long: `Expand a pipeline template into a parent issue plus staged child
issues wired with blocked_by dependencies. Stage 1 starts immediately when its
advance mode is auto; orchestrator_review stages wait for "pipeline advance".
Called from inside a chat task, the run auto-binds to the current chat
session so the chat bridge reports progress back into the conversation.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		templateID, _ := cmd.Flags().GetString("template")
		if templateID == "" {
			return fmt.Errorf("--template is required")
		}
		body := map[string]any{}
		for flag, key := range map[string]string{"title": "title", "description": "description", "session": "orchestrator_session_id"} {
			if v, _ := cmd.Flags().GetString(flag); v != "" {
				body[key] = v
			}
		}
		client, err := newAPIClient(cmd)
		if err != nil {
			return err
		}
		ctx, cancel := cli.APIContext(cmd.Context())
		defer cancel()
		var out json.RawMessage
		if err := client.PostJSON(ctx, "/api/pipeline-templates/"+templateID+"/instantiate", body, &out); err != nil {
			return fmt.Errorf("instantiate pipeline: %w", err)
		}
		return printPipelineJSON(out)
	},
}

var pipelineAdvanceCmd = &cobra.Command{
	Use:   "advance --root <issueId> --stage <n>",
	Short: "Promote a review stage's parked children to active",
	Long: `Orchestrator-only: after the acceptance check (and any human gate)
passes, promote the stage's parked children so their runs start through the
dependency gate.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		root, _ := cmd.Flags().GetString("root")
		stage, _ := cmd.Flags().GetInt32("stage")
		if root == "" || stage < 1 {
			return fmt.Errorf("--root and --stage (>= 1) are required")
		}
		client, err := newAPIClient(cmd)
		if err != nil {
			return err
		}
		ctx, cancel := cli.APIContext(cmd.Context())
		defer cancel()
		var out json.RawMessage
		if err := client.PostJSON(ctx, "/api/pipeline-runs/"+root+"/advance", map[string]any{"stage": stage}, &out); err != nil {
			return fmt.Errorf("advance pipeline: %w", err)
		}
		return printPipelineJSON(out)
	},
}

func printPipelineJSON(v json.RawMessage) error {
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(pretty))
	return nil
}

func init() {
	pipelineInstantiateCmd.Flags().String("template", "", "Pipeline template id (required)")
	pipelineInstantiateCmd.Flags().String("title", "", "Root issue title (the research goal)")
	pipelineInstantiateCmd.Flags().String("description", "", "Root issue description")
	pipelineInstantiateCmd.Flags().String("session", "", "Chat session id to bind (default: the current chat task's session)")
	_ = pipelineInstantiateCmd.MarkFlagRequired("template")
	pipelineAdvanceCmd.Flags().String("root", "", "Pipeline root issue id (required)")
	pipelineAdvanceCmd.Flags().Int32("stage", 0, "Stage number to promote (required)")
	_ = pipelineAdvanceCmd.MarkFlagRequired("root")
	_ = pipelineAdvanceCmd.MarkFlagRequired("stage")

	pipelineCmd.AddCommand(pipelineTemplatesCmd, pipelineInstantiateCmd, pipelineAdvanceCmd)
	rootCmd.AddCommand(pipelineCmd)
}
