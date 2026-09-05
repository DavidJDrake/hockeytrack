variable "region" {
  type    = string
  default = "us-east-1"
}

variable "image_tag" {
  type        = string
  description = "ECR image tag (git SHA) all three Lambdas run"
}

# No default on purpose: a plan without terraform.tfvars must fail rather than
# silently destroy the subscription. Set it to "" to opt out explicitly.
variable "alert_email" {
  type        = string
  description = "Email for CloudWatch alarm notifications; empty string skips the subscription"
}
