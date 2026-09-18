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

variable "enable_foreign_bucket_deny" {
  description = "Add a bucket-policy deny for the two foreign project identities. Requires an MFA session to apply, because the bucket's own policy refuses PutBucketPolicy without one (HOC-53, HOC-58)."
  type        = bool
  default     = false
}

# The CloudFront distribution in front of the scoreboard image mirror
# (images.scoreboard.davidjdrake.com). No default: security-alarms.tf section 15
# watches this distribution by ID, and a rule that silently watched the wrong
# one -- or none -- would be worse than a plan that fails. The value is not a
# secret, only environment-specific, so it lives in terraform.tfvars beside
# alert_email rather than in this repository. Section 15's preconditions check
# both its shape and that the distribution it names really serves the image
# host, so a typo fails at plan rather than at three in the morning.
variable "scoreboard_images_distribution_id" {
  type        = string
  description = "CloudFront distribution ID serving images.scoreboard.davidjdrake.com, the scoreboard device-image mirror"

  # Section 15 carries the same shape check as a precondition, but a
  # precondition on the rule is evaluated after the data source that reads the
  # distribution, so a malformed value fails first with the provider's
  # "couldn't find resource" instead of a sentence saying what to fix. This
  # block runs before any data source and is the message someone actually sees;
  # the precondition remains as the check that travels with the rule.
  validation {
    condition     = can(regex("^E[A-Z0-9]+$", var.scoreboard_images_distribution_id))
    error_message = "scoreboard_images_distribution_id must be a CloudFront distribution ID (^E[A-Z0-9]+$), for example E1GT880VF9CHFS. Set it in terraform.tfvars."
  }
}
