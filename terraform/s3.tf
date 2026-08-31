resource "aws_s3_bucket" "raw" {
  bucket = "hockeytrack-raw-${data.aws_caller_identity.current.account_id}"
}

resource "aws_s3_bucket_public_access_block" "raw" {
  bucket                  = aws_s3_bucket.raw.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
