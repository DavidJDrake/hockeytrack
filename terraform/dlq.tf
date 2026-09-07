# Three Lambda DLQs plus one for the ECR scan-findings EventBridge target
# (ecr-alerts.tf) and one for the security alert targets (security-alarms.tf).
# Every queue here gets a depth alarm from alarms.tf, which is what makes a
# dropped security alert visible rather than merely absent.
#
# Adding a key to this for_each gives aws_sqs_queue.dlq a pending create, which
# defers every data source that reads it -- so the three Lambda role policies in
# iam.tf re-render as "known after apply" and get rewritten with identical
# content on that one apply. It is cosmetic and it settles afterwards. Worth
# knowing before assuming an unexplained IAM diff means something changed.
resource "aws_sqs_queue" "dlq" {
  for_each                  = toset(["schedule-sync", "poller", "sweeper", "ecr-scan-findings", "security-alerts"])
  name                      = "hockeytrack-${each.key}-dlq"
  message_retention_seconds = 1209600 # 14 days
}
