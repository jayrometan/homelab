terraform {
  required_version = ">=1.3.3"

  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "2.25.2"
    }
    null = {
      source  = "hashicorp/null"
      version = "3.2.2"
    }
  }
}