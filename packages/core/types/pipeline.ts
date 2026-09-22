export interface PipelineTemplateStage {
  id: string;
  stage_order: number;
  name: string;
  agent_id: string;
  skill_ids: string[];
  prompt_template: string;
  acceptance_criteria: string;
  advance_mode: string;
  requires_human_gate: boolean;
}

export interface PipelineTemplate {
  id: string;
  workspace_id: string;
  name: string;
  version: number;
  description: string;
  orchestrator_agent_id: string;
  stages: PipelineTemplateStage[];
}

export interface PipelineRunChild {
  issue_id: string;
  stage_order: number;
  name: string;
  agent_id: string;
  status: string;
  advance_mode: string;
}

export interface InstantiatePipelineResponse {
  root_issue_id: string;
  children: PipelineRunChild[];
}

export interface AdvancePipelineResponse {
  promoted: number;
}

export interface IssueDependencyItem {
  issue_id: string;
  title: string;
  status: string;
  resolved: boolean;
}

export interface IssueDependenciesResponse {
  blocked_by: IssueDependencyItem[];
  blocks: IssueDependencyItem[];
}

export interface PipelineTemplateStageRequest {
  stage_order: number;
  name: string;
  agent_id: string;
  skill_ids?: string[];
  prompt_template?: string;
  acceptance_criteria?: string;
  advance_mode?: string;
  requires_human_gate?: boolean;
}

export interface CreatePipelineTemplateRequest {
  name: string;
  version?: number;
  description?: string;
  orchestrator_agent_id?: string;
  stages: PipelineTemplateStageRequest[];
}

export interface UpdatePipelineTemplateRequest {
  name?: string;
  version?: number;
  description?: string;
  orchestrator_agent_id?: string;
  stages?: PipelineTemplateStageRequest[];
}

export interface InstantiatePipelineRequest {
  title?: string;
  description?: string;
  orchestrator_session_id?: string;
}

export interface AdvancePipelineRequest {
  stage: number;
}
