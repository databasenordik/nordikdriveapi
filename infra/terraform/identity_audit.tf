variable "enable_identity" {
  type    = bool
  default = false
}
variable "enable_audit" {
  type    = bool
  default = false
}
variable "identity_image" {
  type    = string
  default = null
  validation {
    condition     = var.identity_image == null || can(regex("/nordik_identity_go@sha256:[a-f0-9]{64}$", var.identity_image))
    error_message = "Identity requires its own immutable image digest."
  }
}
variable "audit_image" {
  type    = string
  default = null
  validation {
    condition     = var.audit_image == null || can(regex("/nordik_audit_go@sha256:[a-f0-9]{64}$", var.audit_image))
    error_message = "Audit requires its own immutable image digest."
  }
}
variable "enable_identity_consumers" {
  type    = bool
  default = false
}
variable "identity_public_enabled" {
  type    = bool
  default = false
}
variable "audit_public_enabled" {
  type    = bool
  default = false
}
variable "identity_tables_relocated" {
  type    = bool
  default = false
}
variable "audit_tables_relocated" {
  type    = bool
  default = false
}
variable "identity_outbox_exclusive" {
  type    = bool
  default = false
}
variable "identity_signup_cleanup" {
  type    = bool
  default = false
}
variable "enable_audit_events" {
  type    = bool
  default = false
}
variable "audit_queues_enabled" {
  type    = bool
  default = false
}

locals {
  identity_runtime      = local.runtime_enabled && var.enable_identity
  audit_runtime         = local.runtime_enabled && var.enable_audit
  extracted_connections = (local.identity_runtime ? 1 : 0) + (local.audit_runtime ? 1 : 0)
  identity_url          = var.project_number != null ? "https://${var.name_prefix}-identity-${var.project_number}.${var.region}.run.app" : ""
  audit_url             = var.project_number != null ? "https://${var.name_prefix}-audit-${var.project_number}.${var.region}.run.app" : ""
  worker_url            = var.project_number != null ? "https://${var.name_prefix}-worker-${var.project_number}.${var.region}.run.app" : ""
  extracted_services = merge(
    local.identity_runtime ? { identity = var.identity_image } : {},
    local.audit_runtime ? { audit = var.audit_image } : {},
  )
  audit_producer_env = var.enable_audit_events ? {
    AUDIT_EVENTS_ENABLED = "true"
    AUDIT_URL            = local.audit_url
    AUDIT_QUEUE          = "projects/${var.project_id}/locations/${var.region}/queues/${var.name_prefix}-audit"
    AUDIT_TASK_ACCOUNT   = google_service_account.runtime["audit-tasks"].email
    AUDIT_READER_ACCOUNT = google_service_account.runtime["audit"].email
    AUDIT_WORKER_ACCOUNT = google_service_account.runtime["worker"].email
  } : {}
  identity_env = var.enable_identity ? merge(local.notification_producer_env, local.notification_event_env, local.audit_producer_env, {
    APP_ROLE                           = "identity"
    IDENTITY_URL                       = local.identity_url
    IDENTITY_PROFILE_ACCOUNTS          = join(",", [for name in concat(["legacy", "worker"], var.enable_audit ? ["audit"] : []) : google_service_account.runtime[name].email])
    IDENTITY_PRINCIPAL_ACCOUNTS        = join(",", [for name in ["legacy", "worker"] : google_service_account.runtime[name].email])
    IDENTITY_EMAIL_ACCOUNTS            = join(",", [for name in ["legacy", "worker"] : google_service_account.runtime[name].email])
    IDENTITY_WORKER_ACCOUNT            = google_service_account.runtime["worker"].email
    NOTIFICATION_OUTBOX_READER_ACCOUNT = google_service_account.runtime["notifications"].email
    SIGNUP_REQUIRE_VERIFIED_EMAIL      = tostring(var.signup_require_verified_email)
    SIGNUP_VERIFICATION_KEY_VERSION    = var.signup_verification_key_version
  }) : {}
  audit_env = var.enable_audit ? {
    APP_ROLE                  = "audit"
    IDENTITY_URL              = local.identity_url
    AUDIT_URL                 = local.audit_url
    AUDIT_TASK_ACCOUNT        = google_service_account.runtime["audit-tasks"].email
    AUDIT_LEGACY_SOURCE_URL   = local.notification_outbox_source_url
    AUDIT_IDENTITY_SOURCE_URL = local.identity_url
  } : {}
  extracted_backends = merge(var.service_backends,
    var.identity_public_enabled ? { identity = { url = local.identity_url, audience = local.identity_url } } : {},
    var.audit_public_enabled ? { audit = { url = local.audit_url, audience = local.audit_url } } : {},
  )
  ownership_backend_env = {
    legacy = merge(var.enable_identity_consumers ? { IDENTITY_URL = local.identity_url } : {}, local.audit_producer_env,
      var.enable_audit_events ? { AUDIT_OWNER_URL = local.notification_outbox_source_url } : {},
      var.identity_public_enabled ? { LEGACY_IDENTITY_PUBLIC_DISABLED = "true" } : {},
      var.audit_public_enabled ? { LEGACY_AUDIT_PUBLIC_DISABLED = "true" } : {},
      var.enable_identity_consumers && !var.identity_public_enabled ? { DB_SCHEMA = "public,identity" } : {},
      var.enable_audit_events && !var.audit_public_enabled ? { DB_SCHEMA = "public,identity,audit" } : {},
    )
    worker = merge(var.enable_identity_consumers ? { IDENTITY_URL = local.identity_url, IDENTITY_NOTIFICATION_DISPATCH_URL = local.identity_url } : {},
      var.enable_audit_events ? { AUDIT_SOURCE_URLS = "${local.notification_outbox_source_url},${local.identity_url}" } : {},
      var.rollout_inspection_enabled ? { ROLLOUT_DEPLOYER_ACCOUNT = var.rollout_deployer_account, WORKER_INSPECTION_AUDIENCE = local.worker_url, ROLLOUT_DATABASE_OWNERS = jsonencode({ for owner in ["gateway", "legacy", "worker", "notifications", "identity", "audit"] : "${var.name_prefix}-${owner}@${var.project_id}.iam" => owner }) } : {},
    )
  }
  notifications_identity_env = var.enable_identity ? {
    NOTIFICATION_OUTBOX_IDENTITY_URL = local.identity_url
    NOTIFICATIONS_IDENTITY_ACCOUNT   = google_service_account.runtime["identity"].email
    NOTIFICATIONS_IDENTITY_EXCLUSIVE = tostring(var.identity_outbox_exclusive)
  } : {}
}

resource "terraform_data" "identity_audit_safeguards" {
  lifecycle {
    precondition {
      condition     = !var.enable_identity || (var.enable_notifications && var.notification_delivery_enabled && var.enable_event_driven_notifications && var.signup_require_verified_email && var.serialized_runtime_rollouts && var.project_number != null)
      error_message = "Identity requires accepted M3 Notifications and verified serialized runtime rollouts."
    }
    precondition {
      condition     = !local.identity_runtime || (var.identity_image != null && contains(keys(lookup(var.runtime_secrets, "identity", {})), "NOTIFICATION_PAYLOAD_KEYS") && contains(keys(lookup(var.runtime_secrets, "identity", {})), "SIGNUP_VERIFICATION_KEYS"))
      error_message = "Pin the Identity image and both existing purpose-specific secret versions."
    }
    precondition {
      condition     = !var.enable_audit || (var.enable_identity && var.enable_identity_consumers && var.serialized_runtime_rollouts)
      error_message = "Extract Identity and migrate its consumers before adding Audit."
    }
    precondition {
      condition     = !local.audit_runtime || var.audit_image != null
      error_message = "Pin the reviewed Audit image."
    }
    precondition {
      condition     = !var.enable_identity_consumers || var.enable_identity
      error_message = "Provision Identity before its consumers."
    }
    precondition {
      condition     = !var.identity_public_enabled || var.enable_identity_consumers
      error_message = "Verify owner-API consumers before moving public Identity routes."
    }
    precondition {
      condition     = !var.enable_audit_events || (var.enable_audit && var.identity_public_enabled)
      error_message = "Audit events require both owner sources and completed public Identity cutover."
    }
    precondition {
      condition     = !var.audit_public_enabled || var.enable_audit_events
      error_message = "Verify Audit ingestion before moving public activity queries."
    }
    precondition {
      condition     = !var.identity_tables_relocated || (var.identity_public_enabled && var.identity_outbox_exclusive && var.identity_signup_cleanup)
      error_message = "Drain legacy auth/dispatchers and transfer the existing cleanup job before Identity relocation."
    }
    precondition {
      condition     = !var.audit_tables_relocated || (var.audit_public_enabled && var.enable_audit_events)
      error_message = "Deploy compatible activity producers/queries before historical table relocation."
    }
    precondition {
      condition     = !var.identity_outbox_exclusive || (var.enable_identity && var.identity_public_enabled)
      error_message = "Identity-only producer grants require the controlled auth/outbox cutover."
    }
    precondition {
      condition     = !var.identity_signup_cleanup || (var.enable_identity && var.enable_signup_cleanup)
      error_message = "Transfer the existing signup cleanup job; do not create a second job."
    }
    precondition {
      condition     = !var.audit_queues_enabled || var.enable_audit_events
      error_message = "Enable Audit dispatch only after producer/source acceptance."
    }
    precondition {
      condition     = alltrue([for key in keys(lookup(var.runtime_secrets, "audit", {})) : key == "JWT_SECRET"]) && alltrue([for key in keys(lookup(var.runtime_secrets, "identity", {})) : contains(["JWT_SECRET", "NOTIFICATION_PAYLOAD_KEYS", "SIGNUP_VERIFICATION_KEYS"], key)])
      error_message = "Audit receives only the existing JWT; Identity receives only its auth and outbox keys. Neither receives SMTP credentials."
    }
  }
}

resource "google_cloud_run_v2_service" "owner" {
  for_each            = local.extracted_services
  project             = var.project_id
  name                = "${var.name_prefix}-${each.key}"
  location            = var.region
  deletion_protection = true
  ingress             = "INGRESS_TRAFFIC_ALL"
  custom_audiences    = [each.key == "identity" ? local.identity_url : local.audit_url]
  scaling { max_instance_count = 1 }
  dynamic "traffic" {
    for_each = contains(keys(var.runtime_serving_revisions), each.key) ? [1] : []
    content {
      type     = "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"
      revision = var.runtime_serving_revisions[each.key]
      percent  = 100
    }
  }
  dynamic "traffic" {
    for_each = contains(keys(var.runtime_candidate_revisions), each.key) ? [1] : []
    content {
      type     = "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"
      revision = var.runtime_candidate_revisions[each.key]
      percent  = 0
      tag      = "m4-candidate"
    }
  }
  template {
    revision                         = lookup(var.runtime_candidate_revisions, each.key, lookup(var.runtime_template_revisions, each.key, null))
    service_account                  = google_service_account.runtime[each.key].email
    timeout                          = "40s"
    max_instance_request_concurrency = 8
    scaling {
      min_instance_count = 0
      max_instance_count = 1
    }
    dynamic "vpc_access" {
      for_each = var.sql_ip_type == "private" ? [1] : []
      content {
        egress = "PRIVATE_RANGES_ONLY"
        network_interfaces {
          network    = var.vpc_network
          subnetwork = var.vpc_subnetwork
        }
      }
    }
    containers {
      image = each.value
      ports {
        name           = each.key == "identity" ? "h2c" : "http1"
        container_port = 8080
      }
      resources {
        limits   = { cpu = "1", memory = "512Mi" }
        cpu_idle = true
      }
      dynamic "env" {
        for_each = merge(lookup(var.runtime_settings, each.key, {}), local.common_env, each.key == "identity" ? local.identity_env : local.audit_env, {
          DB_USER                   = google_sql_user.iam[each.key].name
          DB_MAX_OPEN               = "1"
          DB_SCHEMA                 = each.key == "identity" ? (var.identity_tables_relocated && var.enable_audit_events ? "identity" : "identity,public") : (var.audit_tables_relocated ? "audit,identity" : "audit,public,identity")
          OPERATOR_TOKEN_AUDIENCE   = "https://${var.name_prefix}-${each.key}.internal"
          OPERATOR_SERVICE_ACCOUNTS = join(",", var.operator_accounts)
        })
        content {
          name  = env.key
          value = env.value
        }
      }
      dynamic "env" {
        for_each = local.secrets[each.key]
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.runtime[env.value.secret_id].secret_id
              version = env.value.version
            }
          }
        }
      }
      startup_probe {
        http_get { path = "/healthz" }
        period_seconds    = 3
        failure_threshold = 30
      }
    }
  }
  depends_on = [terraform_data.runtime_staging_safeguards, terraform_data.safeguards, terraform_data.identity_audit_safeguards, terraform_data.serialized_rollout_safeguards, google_project_iam_member.sql, google_secret_manager_secret_iam_member.runtime]
  lifecycle { prevent_destroy = true }
}

resource "google_cloud_run_v2_service_iam_member" "identity_callers" {
  for_each = local.identity_runtime ? toset(concat(["gateway", "legacy", "worker", "notifications"], var.enable_audit ? ["audit"] : [])) : toset([])
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.owner["identity"].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime[each.key].email}"
}
resource "google_cloud_run_v2_service_iam_member" "audit_callers" {
  for_each = local.audit_runtime ? toset(["gateway", "audit-tasks"]) : toset([])
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.owner["audit"].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime[each.key].email}"
}
resource "google_cloud_run_v2_service_iam_member" "audit_legacy_reader" {
  for_each = local.audit_runtime ? toset(["audit", "worker"]) : toset([])
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.backend["legacy"].name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.runtime[each.key].email}"
}

output "identity_url" { value = local.identity_runtime ? local.identity_url : null }
output "audit_url" { value = local.audit_runtime ? local.audit_url : null }
