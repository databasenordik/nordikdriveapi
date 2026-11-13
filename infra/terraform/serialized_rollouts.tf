variable "serialized_runtime_rollouts" {
  type        = bool
  default     = false
  description = "Use a single replacement allowance only after the persistent coordinator has passed its live metadata/CAS proof."
}
variable "rollout_inspection_enabled" {
  type    = bool
  default = false
}
variable "rollout_deployer_account" {
  type        = string
  default     = null
  description = "Existing GitHub deployment service account; the sole caller of the aggregate Worker inspection endpoint."
}
variable "rollout_window_token" {
  type        = string
  default     = null
  sensitive   = true
  description = "Current persistent rollout window identifier. Required for Terraform under serialized runtime capacity."
}
data "google_cloud_run_v2_service" "rollout_marker" {
  count    = var.serialized_runtime_rollouts || length(var.runtime_candidate_revisions) > 0 ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = coalesce(var.public_gateway_service_name, "${var.name_prefix}-gateway")
}
locals {
  current_rollout_marker = try(jsondecode(data.google_cloud_run_v2_service.rollout_marker[0].annotations["nordikdrive.io/runtime-rollout"]), {})
}
resource "terraform_data" "serialized_rollout_safeguards" {
  lifecycle {
    precondition {
      condition     = !var.rollout_inspection_enabled || (var.project_number != null && var.rollout_deployer_account != null)
      error_message = "Worker inspection requires a verified project number and explicit GitHub deployment account."
    }
    precondition {
      condition     = !var.serialized_runtime_rollouts || (var.rollout_inspection_enabled && !var.enable_preview_gateway && alltrue([for value in values(var.max_instances) : value == 1]))
      error_message = "Serialized capacity requires verified inspection and one instance of each development service with no extra preview gateway."
    }
    precondition {
      condition     = !var.serialized_runtime_rollouts || try(local.current_rollout_marker.version == 1 && local.current_rollout_marker.verified && contains(["held", "staged", "tested", "promoted", "recovering"], local.current_rollout_marker.state) && local.current_rollout_marker.token == var.rollout_window_token && var.rollout_window_token != null, false)
      error_message = "Terraform mutations require ownership of a live, verified persistent rollout window. Interrupted windows require explicit recovery."
    }
  }
}
resource "google_cloud_run_v2_service_iam_member" "rollout_inspector" {
  count    = local.runtime_enabled && var.rollout_inspection_enabled ? 1 : 0
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.backend["worker"].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${var.rollout_deployer_account}"
}
