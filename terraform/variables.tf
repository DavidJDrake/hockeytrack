variable "region" {
  type    = string
  default = "us-east-1"
}

variable "image_tag" {
  type        = string
  description = "ECR image tag (git SHA) all three Lambdas run"
}

# Must stay above the number of tagged images currently in the repository, or
# the first apply expires the oldest ones. 20 leaves headroom over the 11 held
# when the policy was introduced (HOC-36).
variable "ecr_keep_images" {
  type        = number
  default     = 20
  description = "Number of most-recent tagged images the ECR lifecycle policy keeps; older ones expire"

  validation {
    condition     = var.ecr_keep_images >= 1 && floor(var.ecr_keep_images) == var.ecr_keep_images
    error_message = "ecr_keep_images must be a positive whole number."
  }
}

# No default on purpose: a plan without terraform.tfvars must fail rather than
# silently destroy the subscription. Set it to "" to opt out explicitly.
variable "alert_email" {
  type        = string
  description = "Email for CloudWatch alarm notifications; empty string skips the subscription"
}

variable "cloudtrail_retention_days" {
  description = "How long to keep CloudTrail logs. Long enough to investigate something noticed late, short enough to bound the bill."
  type        = number
  default     = 365
  validation {
    condition     = var.cloudtrail_retention_days >= 90
    error_message = "Keep at least 90 days: a compromise is often noticed long after it starts."
  }
}

variable "cloudtrail_archive_data_events" {
  description = "Log object-level events on the raw archive, not just management events. This is what makes destruction of the archive visible."
  type        = bool
  default     = true
}

variable "cloudtrail_archive_read_events" {
  description = "Widen archive data events from writes to reads as well. Reads are far more numerous, so this costs more; it buys exfiltration detection."
  type        = bool
  default     = false
}
