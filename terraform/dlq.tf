resource "aws_sqs_queue" "dlq" {
  for_each                  = toset(["schedule-sync", "poller", "sweeper"])
  name                      = "hockeytrack-${each.key}-dlq"
  message_retention_seconds = 1209600 # 14 days
}
