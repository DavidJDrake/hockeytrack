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
    condition     = var.ecr_keep_images >= 1
    error_message = "ecr_keep_images must be at least 1."
  }
}

# No default on purpose: a plan without terraform.tfvars must fail rather than
# silently destroy the subscription. Set it to "" to opt out explicitly.
variable "alert_email" {
  type        = string
  description = "Email for CloudWatch alarm notifications; empty string skips the subscription"
}
