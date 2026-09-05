terraform {
  required_version = ">= 1.6"

  # Remote state (see state.tf). Bucket names cannot use variables here, so
  # the account id is literal.
  backend "s3" {
    bucket         = "hockeytrack-tfstate-989232581535"
    key            = "hockeytrack/terraform.tfstate"
    region         = "us-east-1"
    dynamodb_table = "hockeytrack-tflock"
    encrypt        = true
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
