variable "db_pool_limits" {
  type = object({
    gateway = number
    legacy  = number
    worker  = number
    cleanup = number
  })
  nullable    = false
  default     = { gateway = 4, legacy = 25, worker = 10, cleanup = 4 }
  description = "Per-instance SQL pool limits, including the separate cleanup process. Override with the reviewed environment budget."
  validation {
    condition = alltrue([
      for pool in values(var.db_pool_limits) : pool >= 1 && floor(pool) == pool
    ]) && var.db_pool_limits.worker >= 2
    error_message = "Pools must be positive integers; the worker needs at least two connections for its advisory-lock session and business queries."
  }
}

variable "rollout_extra_instances" {
  type = object({
    gateway = number
    legacy  = number
    worker  = number
  })
  nullable    = false
  default     = { gateway = 1, legacy = 1, worker = 1 }
  description = "Additional instances outside steady-state scaling limits reserved for replacement or overlapping revisions with these same pool limits. Account differently sized old revisions in other_reserved_connections."
  validation {
    condition = alltrue([
      for count in values(var.rollout_extra_instances) : count >= 0 && floor(count) == count
    ])
    error_message = "Additional rollout instance counts must be non-negative integers."
  }
}

variable "database_superuser_reserved_connections" {
  type        = number
  default     = 3
  description = "Verified PostgreSQL superuser_reserved_connections; do not allocate these slots to ordinary runtime users."
  validation {
    condition = (
      var.database_superuser_reserved_connections >= 0 &&
      floor(var.database_superuser_reserved_connections) == var.database_superuser_reserved_connections &&
      var.database_superuser_reserved_connections < var.database_connection_capacity
    )
    error_message = "Reserved slots must be a non-negative integer smaller than the verified total capacity."
  }
}

variable "connection_budget_reviewed" {
  type        = bool
  default     = false
  nullable    = false
  description = "Set true after reviewing the verified instance capacity, controlled runtime limits, rollout overlap and explicit allocations for unchanged clients. Outside allocations are planning budgets, not database-enforced quotas."
}

variable "worker_request_concurrency" {
  type        = number
  default     = 3
  description = "Cloud Run worker request concurrency. The small-database draft uses one job at a time with a two-connection pool."
  validation {
    condition = (
      var.worker_request_concurrency >= 1 &&
      var.worker_request_concurrency <= 1000 &&
      floor(var.worker_request_concurrency) == var.worker_request_concurrency &&
      var.worker_request_concurrency * 2 <= var.db_pool_limits.worker
    )
    error_message = "Worker concurrency must be an integer from 1 to 1000 and leave two pool connections per concurrent job."
  }
}

output "connection_budget" {
  value = {
    total_capacity             = var.database_connection_capacity
    superuser_reserved         = var.database_superuser_reserved_connections
    ordinary_connection_limit  = var.database_connection_capacity - var.database_superuser_reserved_connections
    allocation_limit           = local.connection_budget_limit
    steady_runtime             = local.runtime_connections
    rollout_overlap            = local.rollout_connections
    cleanup                    = var.db_pool_limits.cleanup
    signup_cleanup             = local.signup_cleanup_connections
    existing_and_other_clients = var.other_reserved_connections
    total_allocated            = local.allocated_connections
    remaining_allocation       = local.connection_budget_limit - local.allocated_connections
    budget_reviewed            = var.connection_budget_reviewed
    gateway_deployments        = local.gateway_deployment_count
  }
}
