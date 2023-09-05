terraform {
  required_providers {
    kaniko = {
      version = "0.3.1"
      source  = "registry.terraform.io/seal-io/kaniko"
    }
  }
}

provider "kaniko" {
}

resource "kaniko_image" "example" {
  context     = "git://gitlab-ee.treesir.pub/demotest/walrus/simple-web-service"
  dockerfile  = "Dockerfile-1"
  destination = "harbor.treesir.pub/yangzun/simple-web-service:pod-1"
  build_arg = {
  }
  cache             = false
  no_push           = false
  reproducible      = false
  registry_password = "7opSfnsThQU9CYvqHoVL"
  registry_username = "yangzun"
}
