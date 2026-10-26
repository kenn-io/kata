# Container image definition. Releases publish the oci-* targets as
# ghcr.io/kenn-io/kata.
variable "VERSION" { default = "dev" }
variable "OCI_VERSION" { default = "dev" }
variable "COMMIT" { default = "unknown" }
variable "REVISION" { default = "unknown" }
variable "BUILD_DATE" { default = "" }
variable "OCI_OUTPUT_DIR" { default = "./dist" }

group "default" {
  targets = ["image"]
}

target "image" {
  context    = "."
  dockerfile = "docker/release/Dockerfile"

  args = {
    VERSION    = VERSION
    COMMIT     = COMMIT
    BUILD_DATE = BUILD_DATE
  }

  labels = {
    "org.opencontainers.image.created"  = BUILD_DATE
    "org.opencontainers.image.licenses" = "MIT"
    "org.opencontainers.image.revision" = REVISION
    "org.opencontainers.image.source"   = "https://github.com/kenn-io/kata"
    "org.opencontainers.image.version"  = OCI_VERSION
  }
}

target "oci" {
  name     = "oci-${arch}"
  inherits = ["image"]
  matrix = {
    arch = ["amd64", "arm64"]
  }
  platforms = ["linux/${arch}"]
  output = [{
    type             = "oci"
    dest             = "${OCI_OUTPUT_DIR}/${arch}.oci.tar"
    "oci-mediatypes" = true
  }]
}
