terraform {
  # 1.10 introduced S3-native state locking (use_lockfile below).
  required_version = ">= 1.10"

  # Remote state (see state.tf). Bucket names cannot use variables here, so
  # the account id is literal. Locking uses the S3-native .tflock object next
  # to the state key; the DynamoDB lock table is deprecated and no longer used.
  backend "s3" {
    bucket       = "hockeytrack-tfstate-989232581535"
    key          = "hockeytrack/terraform.tfstate"
    region       = "us-east-1"
    use_lockfile = true
    encrypt      = true
  }
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}
