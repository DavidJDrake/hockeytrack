# Three Lambda DLQs plus one for the ECR scan-findings EventBridge target
# (ecr-alerts.tf). Every queue here gets a depth alarm from alarms.tf.
resource "aws_sqs_queue" "dlq" {
  for_each                  = toset(["schedule-sync", "poller", "sweeper", "ecr-scan-findings"])
  name                      = "hockeytrack-${each.key}-dlq"
  message_retention_seconds = 1209600 # 14 days
}
