output "raw_bucket" {
  value = aws_s3_bucket.raw.bucket
}

output "event_bus" {
  value = aws_cloudwatch_event_bus.main.name
}

output "ecr_repository_url" {
  value = aws_ecr_repository.main.repository_url
}
