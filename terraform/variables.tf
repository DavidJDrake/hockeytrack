variable "region" {
  type    = string
  default = "us-east-1"
}

variable "image_tag" {
  type        = string
  description = "ECR image tag (git SHA) all three Lambdas run"
}

variable "alert_email" {
  type        = string
  default     = ""
  description = "Email for CloudWatch alarm notifications; empty skips the subscription"
}
